import vm from 'vm';
import { randomBytes } from 'node:crypto';
import {
  alreadyDeclaredError,
  analyzeCell,
  applyEdits,
  bindingNames,
  canRedeclare,
  type CellAnalysis,
  type CellBinding,
  type CellBindingKind,
} from './cell-analysis';

export interface CellEvaluation {
  value: unknown;
}

type PersistentBinding = CellBinding & { initialized: boolean; value?: unknown };

export class CellRuntime {
  private readonly declarations = new Map<string, CellBindingKind>();
  private readonly values = new Map<string, PersistentBinding>();
  private sequence = 0;

  constructor(
    private readonly context: vm.Context,
    private readonly contextGlobal: Record<PropertyKey, unknown>,
  ) {}

  async evaluate(source: string): Promise<CellEvaluation> {
    const cellSequence = this.sequence++;
    const initializationTargetName = `__browser_repl_init_${cellSequence}_${randomBytes(16).toString('hex')}`;
    const initializationTarget = `globalThis[${JSON.stringify(initializationTargetName)}]`;
    const analysis = analyzeCell(source, initializationTarget);
    this.precheck(analysis.bindings);

    const currentNames = bindingNames(analysis.bindings);
    const resultName = analysis.finalExpression ? this.freshResultName(currentNames) : undefined;
    const generated = this.buildSource(analysis, resultName, initializationTarget);

    const module = new vm.SourceTextModule(generated, {
      context: this.context,
      identifier: `browser-repl-cell-${cellSequence}.mjs`,
      // buildSource keeps its accessor prelude on one physical line. This
      // makes generated line 2 correspond to user line 1.
      lineOffset: -1,
      initializeImportMeta(meta) {
        meta.url = 'file:///browser-repl-cell.mjs';
      },
      importModuleDynamically: async (specifier) => {
        const namespace = await import(specifier);
        const names = Object.keys(namespace);
        const imported = new vm.SyntheticModule(names, function () {
          for (const name of names) this.setExport(name, namespace[name]);
        }, { context: this.context, identifier: `browser-repl-import-${specifier}` });
        await imported.link(() => {
          throw new Error('dynamic import module unexpectedly requested a static dependency');
        });
        await imported.evaluate();
        return imported;
      },
    });

    await module.link(() => {
      throw new Error('static import is not supported in the browser REPL');
    });

    this.prepareBindings(analysis.bindings);
    this.register(analysis.bindings);
    const initialization = this.createInitializationTarget(analysis.bindings);
    Object.defineProperty(this.contextGlobal, initializationTargetName, {
      configurable: true,
      enumerable: false,
      value: initialization.target,
    });
    try {
      await module.evaluate();
    } finally {
      // Revoke before deleting the property so code that retained the proxy
      // during evaluation cannot repair a failed lexical initializer later.
      initialization.revoke();
      delete this.contextGlobal[initializationTargetName];
    }

    const value = resultName ? Reflect.get(module.namespace, resultName) : undefined;
    return { value };
  }

  private precheck(bindings: CellBinding[]): void {
    const cellDeclarations = new Map<string, CellBindingKind>();
    for (const binding of bindings) {
      const existing = this.declarations.get(binding.name) ?? cellDeclarations.get(binding.name);
      if (existing !== undefined && !canRedeclare(existing, binding.kind)) {
        throw alreadyDeclaredError(binding.name);
      }
      cellDeclarations.set(binding.name, binding.kind);
    }
  }

  private register(bindings: CellBinding[]): void {
    for (const binding of bindings) this.declarations.set(binding.name, binding.kind);
  }

  private createInitializationTarget(bindings: CellBinding[]): {
    target: Record<PropertyKey, unknown>;
    revoke: () => void;
  } {
    const pending = new Set(
      bindings.filter((binding) => binding.kind !== 'var').map((binding) => binding.name),
    );
    const { proxy, revoke } = Proxy.revocable(Object.create(null), {
      set: (_target, property, value) => {
        if (typeof property !== 'string') return false;
        if (!pending.has(property)) throw new TypeError('persistent binding initialization target is internal');
        const binding = this.values.get(property);
        if (!binding) throw new ReferenceError(`Unknown persistent binding '${property}'`);
        pending.delete(property);
        binding.value = value;
        binding.initialized = true;
        return true;
      },
    });
    return { target: proxy, revoke };
  }

  private prepareBindings(bindings: CellBinding[]): void {
    const current = new Map<string, CellBindingKind>();
    for (const binding of bindings) current.set(binding.name, binding.kind);

    for (const [name, kind] of current) {
      const old = this.values.get(name);
      if (old) {
        old.kind = kind;
        // A var redeclaration does not reset an existing value. Function and
        // class declarations are initialized by generated module code.
        if (kind === 'function' || kind === 'class') {
          old.initialized = false;
          old.value = undefined;
        }
        this.exposeGlobal(old);
        continue;
      }

      const binding: PersistentBinding = {
        name,
        kind,
        initialized: kind === 'var',
        value: undefined,
      };
      this.values.set(name, binding);
      this.exposeGlobal(binding);
    }
  }

  private freshResultName(names: Set<string>): string {
    let candidate = `__browser_repl_result_${this.sequence}`;
    while (names.has(candidate) || this.declarations.has(candidate) || this.values.has(candidate)) candidate += '_';
    return candidate;
  }

  private buildSource(
    analysis: CellAnalysis,
    resultName: string | undefined,
    initializationTarget: string,
  ): string {
    const edits = [...analysis.edits];
    if (analysis.finalExpression && resultName) {
      const { statementStart, statementEnd, expressionStart, expressionEnd } = analysis.finalExpression;
      edits.push({
        start: statementStart,
        end: statementEnd,
        text: `const ${resultName} = (${analysis.source.slice(expressionStart, expressionEnd)});`,
      });
    }
    const body = applyEdits(analysis.source, edits);
    const prelude = analysis.hoistedFunctions
      .map(({ name, alias }) =>
        `Object.defineProperty(${alias}, "name", { value: ${JSON.stringify(name)}, configurable: true }); ` +
        `${initializationTarget}[${JSON.stringify(name)}] = ${alias};`,
      )
      .join(' ');
    return `${prelude}\n${body}${resultName ? `\nexport { ${resultName} };` : ''}`;
  }

  private exposeGlobal(binding: PersistentBinding): void {
    const existing = Object.getOwnPropertyDescriptor(this.contextGlobal, binding.name);
    if (existing?.configurable === false) {
      if (binding.kind !== 'const' && existing.writable) this.contextGlobal[binding.name] = binding.value;
      return;
    }

    Object.defineProperty(this.contextGlobal, binding.name, {
      configurable: false,
      enumerable: true,
      get: () => {
        if (!binding.initialized) throw new ReferenceError(`Cannot access '${binding.name}' before initialization`);
        return binding.value;
      },
      set: (value: unknown) => {
        if (binding.kind !== 'var' && !binding.initialized) {
          throw new ReferenceError(`Cannot access '${binding.name}' before initialization`);
        }
        if (binding.kind === 'const' && binding.initialized) {
          throw new TypeError('Assignment to constant variable.');
        }
        binding.value = value;
        binding.initialized = true;
      },
    });
  }
}
