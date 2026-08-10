import vm from 'vm';
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

/**
 * Evaluates each request as a SourceTextModule while keeping bindings in the
 * context global object. A global accessor is the one binding for a name: old
 * closures, new cells, timers, and direct reads all observe the same value.
 * @prev remains the module-link boundary between cells, but it is no longer a
 * lossy value snapshot used to manufacture competing lexical bindings.
 */
export class CellRuntime {
  private readonly declarations = new Map<string, CellBindingKind>();
  private readonly values = new Map<string, PersistentBinding>();
  private sequence = 0;

  constructor(
    private readonly context: vm.Context,
    private readonly contextGlobal: Record<PropertyKey, unknown>,
  ) {}

  async evaluate(source: string): Promise<CellEvaluation> {
    const analysis = analyzeCell(source);
    this.precheck(analysis.bindings);
    const previousModule = this.makePreviousModule();

    const currentNames = bindingNames(analysis.bindings);
    const resultName = analysis.finalExpression ? this.freshResultName(currentNames) : undefined;
    const generated = this.buildSource(analysis, resultName);

    const module = new vm.SourceTextModule(generated, {
      context: this.context,
      identifier: `browser-repl-cell-${this.sequence++}.mjs`,
      // buildSource keeps its import and function prelude on one physical line.
      // This makes generated line 2 correspond to user line 1.
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

    await module.link(async (specifier) => {
      if (specifier === '@prev') return previousModule;
      throw new Error(`static import is not supported in the browser REPL: ${specifier}`);
    });

    this.prepareBindings(analysis.bindings);
    this.register(analysis.bindings);
    await module.evaluate();

    const value = resultName ? this.readExport(module, resultName).value : undefined;
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

  private prepareBindings(bindings: CellBinding[]): void {
    const current = new Map<string, CellBindingKind>();
    for (const binding of bindings) current.set(binding.name, binding.kind);

    for (const [name, kind] of current) {
      const old = this.values.get(name);
      if (old) {
        old.kind = kind;
        // A var redeclaration does not reset an existing value. Function
        // declarations are assigned by the hoisted generated prelude.
        if (kind === 'function' || kind === 'class' || (kind !== 'var' && !old.initialized)) {
          old.initialized = false;
          old.value = undefined;
        }
        this.exposeGlobal(old);
        continue;
      }

      const binding: PersistentBinding = {
        name,
        kind,
        initialized: kind === 'var' ? true : false,
        value: kind === 'var' ? undefined : undefined,
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

  private makePreviousModule(): vm.SyntheticModule {
    const previous = new Map(this.values);
    const names = [...previous.keys()];
    return new vm.SyntheticModule(names, function () {
      for (const name of names) this.setExport(name, previous.get(name)?.value);
    }, { context: this.context, identifier: 'browser-repl-@prev' });
  }

  private buildSource(analysis: CellAnalysis, resultName: string | undefined): string {
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
    const prelude = [
      'import * as __prev from "@prev"; void __prev;',
      ...analysis.hoistedFunctions.map((functionSource) => `${functionSource};`),
    ].join(' ');
    return `${prelude}\n${body}${resultName ? `\nexport { ${resultName} };` : ''}`;
  }

  private readExport(module: vm.SourceTextModule, name: string): { initialized: boolean; value?: unknown } {
    try {
      return { initialized: true, value: module.namespace[name] };
    } catch {
      return { initialized: false };
    }
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
        if (binding.kind === 'const' && binding.initialized) {
          throw new TypeError('Assignment to constant variable.');
        }
        binding.value = value;
        binding.initialized = true;
      },
    });
  }
}
