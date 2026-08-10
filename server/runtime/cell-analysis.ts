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
  hoistedFunctions: string[];
  finalExpression?: { statementStart: number; statementEnd: number; expressionStart: number; expressionEnd: number };
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
        if (property.type === 'RestElement') collectPatternNames(property.argument as ESTree.Pattern, out);
        else if (property.type === 'Property') collectPatternNames(property.value as ESTree.Pattern, out);
      }
      return;
    case 'ArrayPattern':
      for (const element of pattern.elements) {
        if (element) collectPatternNames(element as ESTree.Pattern, out);
      }
      return;
    case 'AssignmentPattern':
      collectPatternNames(pattern.left as ESTree.Pattern, out);
      return;
    case 'RestElement':
      collectPatternNames(pattern.argument as ESTree.Pattern, out);
      return;
    case 'MemberExpression':
      return;
  }
}

function addVariableBindings(statement: ESTree.VariableDeclaration, bindings: CellBinding[]): void {
  for (const declaration of statement.declarations) {
    const names: string[] = [];
    collectPatternNames(declaration.id, names);
    for (const name of names) bindings.push({ name, kind: statement.kind as CellBindingKind });
  }
}

function propertyText(property: ESTree.Property, source: string): string {
  const [start, end] = range(property.key);
  return source.slice(start, end);
}

/** Turn a binding pattern into an assignment target backed by globalThis. */
function globalPattern(pattern: ESTree.Pattern, source: string): string {
  switch (pattern.type) {
    case 'Identifier':
      return `globalThis[${JSON.stringify(pattern.name)}]`;
    case 'AssignmentPattern':
      return `${globalPattern(pattern.left as ESTree.Pattern, source)} = ${source.slice(...range(pattern.right!))}`;
    case 'RestElement':
      return `...${globalPattern(pattern.argument as ESTree.Pattern, source)}`;
    case 'ArrayPattern':
      return `[${pattern.elements.map((element) => element ? globalPattern(element as ESTree.Pattern, source) : '').join(', ')}]`;
    case 'ObjectPattern':
      return `{${pattern.properties.map((property) => {
        if (property.type === 'RestElement') return globalPattern(property, source);
        if (property.type !== 'Property') throw new Error(`unsupported object pattern property: ${property.type}`);
        const key = propertyText(property, source);
        const target = globalPattern(property.value as ESTree.Pattern, source);
        return `${property.computed ? `[${key}]` : key}: ${target}`;
      }).join(', ')}}`;
    case 'MemberExpression':
      return source.slice(...range(pattern));
  }
}

function variableReplacement(statement: ESTree.VariableDeclaration, source: string, expressionPosition: boolean): string {
  const assignments: string[] = [];
  for (const declaration of statement.declarations) {
    if (!declaration.init && statement.kind === 'var') continue;
    const target = globalPattern(declaration.id, source);
    const value = declaration.init ? source.slice(...range(declaration.init)) : 'undefined';
    assignments.push(`(${target} = ${value})`);
  }
  if (expressionPosition) return assignments.join(', ');
  return assignments.length ? `${assignments.join('; ')};` : '';
}

function preserveLines(source: string): string {
  return source.replace(/[^\n]/g, ' ');
}

function addStatement(
  statement: ESTree.Statement,
  source: string,
  bindings: CellBinding[],
  edits: SourceEdit[],
): void {
  switch (statement.type) {
    case 'VariableDeclaration':
      if (statement.kind === 'var') {
        addVariableBindings(statement, bindings);
        edits.push({ start: range(statement)[0], end: range(statement)[1], text: variableReplacement(statement, source, false) });
      }
      return;
    case 'BlockStatement':
      for (const child of statement.body) addStatement(child, source, bindings, edits);
      return;
    case 'IfStatement':
      addStatement(statement.consequent, source, bindings, edits);
      if (statement.alternate) addStatement(statement.alternate, source, bindings, edits);
      return;
    case 'ForStatement':
      if (statement.init?.type === 'VariableDeclaration' && statement.init.kind === 'var') {
        addVariableBindings(statement.init, bindings);
        edits.push({ start: range(statement.init)[0], end: range(statement.init)[1], text: variableReplacement(statement.init, source, true) });
      }
      addStatement(statement.body, source, bindings, edits);
      return;
    case 'ForInStatement':
    case 'ForOfStatement':
      if (statement.left.type === 'VariableDeclaration' && statement.left.kind === 'var') {
        addVariableBindings(statement.left, bindings);
        // `for (var x of values)` is an assignment target, not an
        // initializer. The declaration is already hoisted by the runtime.
        const target = statement.left.declarations.length === 1
          ? globalPattern(statement.left.declarations[0].id, source)
          : variableReplacement(statement.left, source, true);
        edits.push({ start: range(statement.left)[0], end: range(statement.left)[1], text: target });
      }
      addStatement(statement.body, source, bindings, edits);
      return;
    case 'WhileStatement':
    case 'DoWhileStatement':
    case 'WithStatement':
      addStatement(statement.body, source, bindings, edits);
      return;
    case 'SwitchStatement':
      for (const clause of statement.cases) {
        for (const child of clause.consequent) addStatement(child, source, bindings, edits);
      }
      return;
    case 'TryStatement':
      addStatement(statement.block, source, bindings, edits);
      if (statement.handler) addStatement(statement.handler.body, source, bindings, edits);
      if (statement.finalizer) addStatement(statement.finalizer, source, bindings, edits);
      return;
    case 'LabeledStatement':
      addStatement(statement.body, source, bindings, edits);
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
      return;
    default:
      assertNever(statement);
  }
}

function assertNever(value: never): never {
  throw new Error(`Unhandled Meriyah statement: ${(value as { type: string }).type}`);
}

function declarationEdit(statement: ESTree.ClassDeclaration, source: string): SourceEdit {
  const [start, end] = range(statement);
  return { start, end, text: `globalThis[${JSON.stringify(statement.id!.name)}] = (${source.slice(start, end)});` };
}

export function analyzeCell(source: string): CellAnalysis {
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
  const hoistedFunctions: string[] = [];
  for (const statement of ast.body) {
    if (statement.type === 'VariableDeclaration') {
      addVariableBindings(statement, bindings);
      edits.push({ start: range(statement)[0], end: range(statement)[1], text: variableReplacement(statement, source, false) });
    } else if (statement.type === 'FunctionDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'function' });
      const [start, end] = range(statement);
      hoistedFunctions.push(`globalThis[${JSON.stringify(statement.id.name)}] = (${source.slice(start, end)})`);
      edits.push({ start, end, text: preserveLines(source.slice(start, end)) });
    } else if (statement.type === 'ClassDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'class' });
      edits.push(declarationEdit(statement, source));
    } else {
      addStatement(statement, source, bindings, edits);
    }
  }

  const last = ast.body[ast.body.length - 1];
  return {
    source,
    bindings,
    edits,
    hoistedFunctions,
    finalExpression:
      last?.type === 'ExpressionStatement'
        ? {
            statementStart: range(last)[0],
            statementEnd: range(last)[1],
            expressionStart: range(last.expression)[0],
            expressionEnd: range(last.expression)[1],
          }
        : undefined,
  };
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
