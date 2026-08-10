import vm from 'vm';
import {
  alreadyDeclaredError,
  analyzeCell,
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
 * Owns the small amount of state needed to turn independent SourceTextModule
 * instances into a serial, persistent cell runtime. The modules themselves
 * are deliberately short-lived; @prev is the only bridge between cells.
 */
export class CellRuntime {
  private readonly declarations = new Map<string, CellBindingKind>();
  private readonly values = new Map<string, PersistentBinding>();
  private readonly initialized = new Set<string>();
  private readonly markerName = `__browser_repl_mark_${Math.random().toString(36).slice(2)}__`;
  private sequence = 0;

  constructor(
    private readonly context: vm.Context,
    private readonly contextGlobal: Record<PropertyKey, unknown>,
  ) {
    Object.defineProperty(this.contextGlobal, this.markerName, {
      configurable: false,
      value: (name: string) => this.initialized.add(name),
    });
  }

  async evaluate(source: string): Promise<CellEvaluation> {
    const analysis = analyzeCell(source);
    this.precheck(analysis.bindings);
    this.initialized.clear();

    const previous = new Map(this.values);
    const currentNames = bindingNames(analysis.bindings);
    const resultName = analysis.finalExpression ? this.freshResultName(currentNames) : undefined;
    const generated = this.buildSource(analysis, previous, resultName);
    const previousModule = this.makePreviousModule(previous);

    let module: vm.SourceTextModule;
    try {
      module = new vm.SourceTextModule(generated, {
        context: this.context,
        identifier: `browser-repl-cell-${this.sequence++}.mjs`,
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
    } catch (error) {
      throw error;
    }

    await module.link(async (specifier) => {
      if (specifier === '@prev') return previousModule;
      throw new Error(`static import is not supported in the browser REPL: ${specifier}`);
    });

    // A declaration is instantiated before the first statement runs. Keep
    // those names reserved even if evaluation later fails; initialized
    // exports are committed below, matching partial JavaScript execution.
    this.register(analysis.bindings);

    try {
      await module.evaluate();
    } catch (error) {
      this.commit(module, analysis, previous, resultName, false);
      throw error;
    }

    const value = resultName ? this.readExport(module, resultName).value : undefined;
    this.commit(module, analysis, previous, resultName, true);
    return { value };
  }

  private precheck(bindings: CellBinding[]): void {
    for (const binding of bindings) {
      const existing = this.declarations.get(binding.name);
      if (existing !== undefined && !canRedeclare(existing, binding.kind)) {
        throw alreadyDeclaredError(binding.name);
      }
    }
  }

  private register(bindings: CellBinding[]): void {
    for (const binding of bindings) this.declarations.set(binding.name, binding.kind);
  }

  private freshResultName(names: Set<string>): string {
    let candidate = `__browser_repl_result_${this.sequence}`;
    while (names.has(candidate) || this.declarations.has(candidate)) candidate += '_';
    return candidate;
  }

  private makePreviousModule(previous: Map<string, PersistentBinding>): vm.SyntheticModule {
    const names = [...previous.keys()];
    return new vm.SyntheticModule(names, function () {
      for (const name of names) this.setExport(name, previous.get(name)?.value);
    }, { context: this.context, identifier: 'browser-repl-@prev' });
  }

  private buildSource(
    analysis: CellAnalysis,
    previous: Map<string, PersistentBinding>,
    resultName: string | undefined,
  ): string {
    const current = new Map<string, CellBindingKind>();
    for (const binding of analysis.bindings) current.set(binding.name, binding.kind);

    const imports: string[] = [];
    const aliases = new Map<string, string>();
    let aliasIndex = 0;
    for (const name of previous.keys()) {
      const alias = `__browser_repl_prev_${this.sequence}_${aliasIndex++}`;
      aliases.set(name, alias);
      imports.push(`${name} as ${alias}`);
    }

    const prelude: string[] = [];
    for (const [name, oldBinding] of previous) {
      const incoming = current.get(name);
      // A function declaration is instantiated before statements and replaces
      // a prior var/function binding. Do not assign the old value over it.
      if (incoming === 'function') continue;

      if (incoming === 'var') {
        // var redeclaration without an initializer must retain its prior value.
        prelude.push(`var ${name} = ${aliases.get(name)};`);
        continue;
      }
      if (incoming !== undefined) continue; // lexical redeclaration was rejected
      if (!oldBinding.initialized) continue; // preserve the prior TDZ lazily

      const kind = oldBinding.kind === 'const' ? 'const' : 'let';
      prelude.push(`${kind} ${name} = ${aliases.get(name)};`);
    }

    let body = analysis.source;
    if (analysis.finalExpression && resultName) {
      const { statementStart, statementEnd, expressionStart, expressionEnd } = analysis.finalExpression;
      body = `${analysis.source.slice(0, statementStart)}const ${resultName} = (${analysis.source.slice(expressionStart, expressionEnd)});${analysis.source.slice(statementEnd)}`;
    }
    body = this.addInitializationMarkers(body, analysis, previous);

    const exports = new Set<string>([
      ...[...previous].filter(([, binding]) => binding.initialized).map(([name]) => name),
      ...current.keys(),
    ]);
    if (resultName) exports.add(resultName);
    const exportList = [...exports].map((name) => `${name} as ${name}`).join(', ');

    return [
      imports.length ? `import { ${imports.join(', ')} } from '@prev';` : '',
      ...prelude,
      body,
      exportList ? `export { ${exportList} };` : '',
    ].filter(Boolean).join('\n');
  }

  private addInitializationMarkers(
    source: string,
    analysis: CellAnalysis,
    previous: Map<string, PersistentBinding>,
  ): string {
    const markers: string[] = [...previous]
      .filter(([, binding]) => binding.initialized)
      .map(([name]) => name);
    for (const binding of analysis.bindings) {
      if (binding.kind === 'var') markers.push(binding.name);
    }
    const prefix = markers.map((name) => `;globalThis.${this.markerName}(${JSON.stringify(name)});`).join('');
    if (analysis.declarations.every((declaration) => declaration.kind === 'var')) return prefix + source;

    let result = source;
    for (let i = analysis.declarations.length - 1; i >= 0; i--) {
      const declaration = analysis.declarations[i];
      if (declaration.kind === 'var') continue;
      const marker = declaration.names.map((name) => `;globalThis.${this.markerName}(${JSON.stringify(name)});`).join('');
      result = `${result.slice(0, declaration.end)}${marker}${result.slice(declaration.end)}`;
    }
    return prefix + result;
  }

  private readExport(module: vm.SourceTextModule, name: string): { initialized: boolean; value?: unknown } {
    try {
      return { initialized: true, value: module.namespace[name] };
    } catch {
      return { initialized: false };
    }
  }

  private commit(
    module: vm.SourceTextModule,
    analysis: CellAnalysis,
    previous: Map<string, PersistentBinding>,
    resultName: string | undefined,
    succeeded: boolean,
  ): void {
    const current = new Map<string, CellBindingKind>();
    for (const binding of analysis.bindings) current.set(binding.name, binding.kind);

    for (const name of new Set([...previous.keys(), ...current.keys()])) {
      if (name === resultName) continue;
      const oldBinding = previous.get(name);
      const isCurrent = current.has(name);
      if (oldBinding && !oldBinding.initialized && !isCurrent) continue;
      const read = this.readExport(module, name);
      if (!read.initialized && !isCurrent) continue;
      const kind = current.get(name) ?? oldBinding?.kind ?? 'let';
      const initialized = succeeded || (this.initialized.has(name) && read.initialized);
      const binding: PersistentBinding = { name, kind, initialized, value: initialized ? read.value : undefined };
      this.values.set(name, binding);
      this.exposeGlobal(binding);
    }
  }

  private exposeGlobal(binding: PersistentBinding): void {
    if (!binding.initialized) {
      Object.defineProperty(this.contextGlobal, binding.name, {
        configurable: true,
        enumerable: true,
        get: () => {
          throw new ReferenceError(`Cannot access '${binding.name}' before initialization`);
        },
      });
      return;
    }
    const existing = Object.getOwnPropertyDescriptor(this.contextGlobal, binding.name);
    if (existing && existing.configurable === false) {
      // Mutable module bindings are represented by a non-configurable global
      // property. Update its value instead of trying to redefine it. A const
      // property is intentionally never rewritten.
      if (binding.kind !== 'const') this.contextGlobal[binding.name] = binding.value;
      return;
    }
    if (binding.kind === 'const') {
      Object.defineProperty(this.contextGlobal, binding.name, {
        configurable: false,
        enumerable: true,
        get: () => binding.value,
        set: () => {
          throw new TypeError('Assignment to constant variable.');
        },
      });
      return;
    }
    Object.defineProperty(this.contextGlobal, binding.name, {
      configurable: false,
      enumerable: true,
      writable: true,
      value: binding.value,
    });
  }
}
