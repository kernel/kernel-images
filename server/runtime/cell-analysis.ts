import { parseModule, type ESTree } from 'meriyah';

export type CellBindingKind = 'var' | 'function' | 'let' | 'const' | 'class';

export interface CellBinding {
  name: string;
  kind: CellBindingKind;
}

export interface SourceEdit {
  start: number;
  end: number;
  text: string;
}

export interface CellAnalysis {
  source: string;
  bindings: CellBinding[];
  edits: SourceEdit[];
  hoistedFunctions: Array<{ name: string; alias: string }>;
}

export const STATIC_IMPORT_ERROR =
  'static import/export is not supported in the browser REPL; use dynamic import() instead';
export const TOP_LEVEL_RETURN_ERROR =
  'top-level return is not supported in the browser REPL';

function range(node: ESTree._Node): [number, number] {
  if (node.start === undefined || node.end === undefined) throw new Error(`Meriyah node has no range: ${node}`);
  return [node.start, node.end];
}

function collectPatternNames(pattern: ESTree.Pattern | null, out: string[]): void {
  if (!pattern) return;
  switch (pattern.type) {
    case 'Identifier':
      out.push(pattern.name);
      return;
    case 'ObjectPattern':
      for (const property of pattern.properties) {
        if (property.type === 'RestElement') collectPatternNames(asPattern(property.argument), out);
        else if (property.type === 'Property') collectPatternNames(asPattern(property.value), out);
      }
      return;
    case 'ArrayPattern':
      for (const element of pattern.elements) {
        if (element) collectPatternNames(asPattern(element), out);
      }
      return;
    case 'AssignmentPattern':
      collectPatternNames(asPattern(pattern.left), out);
      return;
    case 'RestElement':
      collectPatternNames(asPattern(pattern.argument), out);
      return;
    case 'MemberExpression':
      return;
  }
}

function addVariableBindings(statement: ESTree.VariableDeclaration, bindings: CellBinding[]): void {
  for (const declaration of statement.declarations) {
    const names: string[] = [];
    collectPatternNames(asPattern(declaration.id), names);
    for (const name of names) bindings.push({ name, kind: statement.kind as CellBindingKind });
  }
}

function propertyText(property: ESTree.Property, source: string): string {
  const [start, end] = range(property.key);
  return source.slice(start, end);
}

function asPattern(node: ESTree.Node): ESTree.Pattern {
  switch (node.type) {
    case 'Identifier':
    case 'ObjectPattern':
    case 'ArrayPattern':
    case 'AssignmentPattern':
    case 'RestElement':
    case 'MemberExpression':
      return node;
    default:
      throw new Error(`unsupported binding pattern: ${node.type}`);
  }
}

const DEFAULT_INITIALIZATION_TARGET = 'globalThis["__browser_repl_init_target"]';

function globalPattern(
  pattern: ESTree.Pattern,
  source: string,
  initialize: boolean,
  initializationTarget: string,
): string {
  switch (pattern.type) {
    case 'Identifier':
      return `${initialize ? initializationTarget : 'globalThis'}[${JSON.stringify(pattern.name)}]`;
    case 'AssignmentPattern':
      return `${globalPattern(asPattern(pattern.left), source, initialize, initializationTarget)} = ${source.slice(...range(pattern.right!))}`;
    case 'RestElement':
      return `...${globalPattern(asPattern(pattern.argument), source, initialize, initializationTarget)}`;
    case 'ArrayPattern':
      return `[${pattern.elements.map((element) => element ? globalPattern(asPattern(element), source, initialize, initializationTarget) : '').join(', ')}]`;
    case 'ObjectPattern':
      return `{${pattern.properties.map((property) => {
        if (property.type === 'RestElement') return globalPattern(asPattern(property), source, initialize, initializationTarget);
        if (property.type !== 'Property') throw new Error(`unsupported object pattern property: ${property.type}`);
        const key = propertyText(property, source);
        const target = globalPattern(asPattern(property.value), source, initialize, initializationTarget);
        return `${property.computed ? `[${key}]` : key}: ${target}`;
      }).join(', ')}}`;
    case 'MemberExpression':
      return source.slice(...range(pattern));
  }
}

function declaratorReplacement(
  declaration: ESTree.VariableDeclarator,
  statement: ESTree.VariableDeclaration,
  source: string,
  initialize: boolean,
  initializationTarget: string,
): string {
  // A `var x;` has already been initialized by prepareBindings and is a
  // no-op. Keep a syntactic expression for it: statement-position lowering
  // must remain one statement even when the declaration has no initializer.
  if (!declaration.init && statement.kind === 'var') return '(void 0)';
  const target = globalPattern(asPattern(declaration.id), source, initialize, initializationTarget);
  const value = declaration.init ? source.slice(...range(declaration.init)) : 'undefined';
  return `(${target} = ${value})`;
}

function variableReplacement(
  statement: ESTree.VariableDeclaration,
  source: string,
  expressionPosition: boolean,
  initialize: boolean,
  initializationTarget: string,
): string {
  const assignments = statement.declarations.map((declaration) =>
    declaratorReplacement(declaration, statement, source, initialize, initializationTarget),
  );
  return expressionPosition ? assignments.join(', ') : `${assignments.join(', ')};`;
}

function variableEdits(
  statement: ESTree.VariableDeclaration,
  source: string,
  edits: SourceEdit[],
  initialize: boolean,
  initializationTarget: string,
): void {
  const first = statement.declarations[0];
  if (!first) return;
  edits.push({ start: statement.start!, end: first.start!, text: '' });
  for (let index = 0; index < statement.declarations.length; index++) {
    const declaration = statement.declarations[index];
    edits.push({
      start: declaration.start!,
      end: declaration.end!,
      text: declaratorReplacement(declaration, statement, source, initialize, initializationTarget),
    });
  }
  // Meriyah includes an explicit semicolon in the declaration range. If the
  // source used ASI, add the terminator needed by a statement-position
  // assignment while leaving all original line breaks untouched.
  if (source[statement.end! - 1] !== ';') {
    edits.push({ start: statement.end!, end: statement.end!, text: ';' });
  }
}

function addStatement(
  statement: ESTree.Statement,
  source: string,
  bindings: CellBinding[],
  edits: SourceEdit[],
  initializationTarget: string,
): void {
  switch (statement.type) {
    case 'VariableDeclaration':
      if (statement.kind === 'var') {
        addVariableBindings(statement, bindings);
        variableEdits(statement, source, edits, false, initializationTarget);
      }
      return;
    case 'BlockStatement':
      for (const child of statement.body) addStatement(child, source, bindings, edits, initializationTarget);
      return;
    case 'IfStatement':
      addStatement(statement.consequent, source, bindings, edits, initializationTarget);
      if (statement.alternate) addStatement(statement.alternate, source, bindings, edits, initializationTarget);
      return;
    case 'ForStatement':
      if (statement.init?.type === 'VariableDeclaration' && statement.init.kind === 'var') {
        addVariableBindings(statement.init, bindings);
        edits.push({
          start: range(statement.init)[0],
          end: range(statement.init)[1],
          text: variableReplacement(statement.init, source, true, false, initializationTarget),
        });
      }
      addStatement(statement.body, source, bindings, edits, initializationTarget);
      return;
    case 'ForInStatement':
    case 'ForOfStatement':
      if (statement.left.type === 'VariableDeclaration' && statement.left.kind === 'var') {
        addVariableBindings(statement.left, bindings);
        // The parser rejects multi-declarator for-in/of heads, so this is
        // always a single assignment target.
        const target = globalPattern(asPattern(statement.left.declarations[0].id), source, false, initializationTarget);
        edits.push({ start: range(statement.left)[0], end: range(statement.left)[1], text: target });
      }
      addStatement(statement.body, source, bindings, edits, initializationTarget);
      return;
    case 'WhileStatement':
    case 'DoWhileStatement':
    case 'WithStatement':
      addStatement(statement.body, source, bindings, edits, initializationTarget);
      return;
    case 'SwitchStatement':
      for (const clause of statement.cases) {
        for (const child of clause.consequent) addStatement(child, source, bindings, edits, initializationTarget);
      }
      return;
    case 'TryStatement':
      addStatement(statement.block, source, bindings, edits, initializationTarget);
      if (statement.handler) addStatement(statement.handler.body, source, bindings, edits, initializationTarget);
      if (statement.finalizer) addStatement(statement.finalizer, source, bindings, edits, initializationTarget);
      return;
    case 'LabeledStatement':
      addStatement(statement.body, source, bindings, edits, initializationTarget);
      return;
    case 'FunctionDeclaration':
    case 'ClassDeclaration':
    case 'EmptyStatement':
    case 'ExpressionStatement':
    case 'BreakStatement':
    case 'ContinueStatement':
    case 'DebuggerStatement':
    case 'ReturnStatement':
    case 'ThrowStatement':
    case 'ImportDeclaration':
    // Meriyah's broad ESTree Statement union includes these declaration forms,
    // but analyzeCell rejects them before traversal. Keep the boundary explicit.
    case 'ClassExpression':
    case 'ExportAllDeclaration':
    case 'ExportDefaultDeclaration':
    case 'ExportNamedDeclaration':
      return;
    default:
      assertNever(statement);
  }
}

function assertNever(value: never): never {
  throw new Error(`Unhandled Meriyah statement: ${(value as { type: string }).type}`);
}

function declarationEdit(statement: ESTree.ClassDeclaration, source: string, initializationTarget: string): SourceEdit {
  const [start, end] = range(statement);
  return {
    start,
    end,
    text: `${initializationTarget}[${JSON.stringify(statement.id!.name)}] = (${source.slice(start, end)});`,
  };
}

export function analyzeCell(
  source: string,
  initializationTarget = DEFAULT_INITIALIZATION_TARGET,
): CellAnalysis {
  let ast: ESTree.Program;
  try {
    ast = parseModule(source, { next: true, ranges: true });
  } catch (error: unknown) {
    const message = String(error instanceof Error ? error.message : error);
    // Meriyah is exact-pinned. Keep this assertion close to the parser boundary
    // so a dependency upgrade cannot silently change the public diagnostic.
    if (/return statement/.test(message)) throw new SyntaxError(TOP_LEVEL_RETURN_ERROR);
    throw error;
  }

  for (const statement of ast.body) {
    if (statement.type === 'ImportDeclaration' || statement.type.startsWith('Export')) {
      throw new SyntaxError(STATIC_IMPORT_ERROR);
    }
  }

  const bindings: CellBinding[] = [];
  const edits: SourceEdit[] = [];
  const functionDeclarations: ESTree.FunctionDeclaration[] = [];
  for (const statement of ast.body) {
    if (statement.type === 'VariableDeclaration') {
      addVariableBindings(statement, bindings);
      if (statement.kind === 'var') variableEdits(statement, source, edits, false, initializationTarget);
      else variableEdits(statement, source, edits, true, initializationTarget);
    } else if (statement.type === 'FunctionDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'function' });
      functionDeclarations.push(statement);
    } else if (statement.type === 'ClassDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'class' });
      edits.push(declarationEdit(statement, source, initializationTarget));
    } else {
      addStatement(statement, source, bindings, edits, initializationTarget);
    }
  }

  // A module-local function binding would shadow the persistent accessor.
  // Rename only the declaration identifier: references in bodies remain free
  // identifiers and therefore resolve through the accessor at call time.
  const usedNames = bindingNames(bindings);
  const hoistedByName = new Map<string, { name: string; alias: string }>();
  for (const statement of functionDeclarations) {
    if (!statement.id) throw new Error('function declaration disappeared during analysis');
    const aliasBase = `__browser_repl_function_${statement.id.name}`;
    let alias = aliasBase;
    while (usedNames.has(alias)) alias += '_';
    usedNames.add(alias);
    edits.push({ start: statement.id.start!, end: statement.id.end!, text: alias });
    // Duplicate function declarations are valid; only the last declaration
    // should initialize the single persistent binding.
    hoistedByName.set(statement.id.name, { name: statement.id.name, alias });
  }
  const hoistedFunctions = [...hoistedByName.values()];

  return { source, bindings, edits, hoistedFunctions };
}

export function applyEdits(source: string, edits: SourceEdit[]): string {
  const ordered = [...edits].sort((a, b) => a.start - b.start);
  let result = '';
  let cursor = 0;
  for (const edit of ordered) {
    if (edit.start < cursor) throw new Error('overlapping cell source edits');
    result += source.slice(cursor, edit.start) + edit.text;
    cursor = edit.end;
  }
  return result + source.slice(cursor);
}

export function bindingNames(bindings: CellBinding[]): Set<string> {
  return new Set(bindings.map((binding) => binding.name));
}

export function isLexicalKind(kind: CellBindingKind): boolean {
  return kind === 'let' || kind === 'const' || kind === 'class';
}

export function canRedeclare(existing: CellBindingKind, incoming: CellBindingKind): boolean {
  return !isLexicalKind(existing) && !isLexicalKind(incoming);
}

export function alreadyDeclaredError(name: string): SyntaxError {
  return new SyntaxError(`Identifier '${name}' has already been declared; use a new name or reset the REPL to retry`);
}
