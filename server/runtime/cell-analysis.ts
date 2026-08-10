import { parseModule } from 'meriyah';

export type CellBindingKind = 'var' | 'function' | 'let' | 'const' | 'class';

export interface CellBinding {
  name: string;
  kind: CellBindingKind;
}

export interface CellAnalysis {
  source: string;
  bindings: CellBinding[];
  declarations: Array<{ start: number; end: number; names: string[]; kind: CellBindingKind }>;
  finalExpression?: { statementStart: number; statementEnd: number; expressionStart: number; expressionEnd: number };
}

export const STATIC_IMPORT_ERROR =
  'static import/export is not supported in the browser REPL; use dynamic import() instead';
export const TOP_LEVEL_RETURN_ERROR =
  'top-level return is not supported in the browser REPL';

function collectPatternNames(pattern: any, out: string[]): void {
  if (!pattern) return;
  switch (pattern.type) {
    case 'Identifier':
      out.push(pattern.name);
      return;
    case 'ObjectPattern':
      for (const property of pattern.properties) {
        collectPatternNames(property.type === 'RestElement' ? property.argument : property.value, out);
      }
      return;
    case 'ArrayPattern':
      for (const element of pattern.elements) collectPatternNames(element, out);
      return;
    case 'AssignmentPattern':
      collectPatternNames(pattern.left, out);
      return;
    case 'RestElement':
      collectPatternNames(pattern.argument, out);
      return;
  }
}

function addVariableBindings(statement: any, bindings: CellBinding[]): void {
  const names: string[] = [];
  for (const declaration of statement.declarations) collectPatternNames(declaration.id, names);
  for (const name of names) bindings.push({ name, kind: statement.kind as CellBindingKind });
}

function collectStatementBindings(statement: any, bindings: CellBinding[]): void {
  if (!statement) return;
  if (statement.type === 'VariableDeclaration' && statement.kind === 'var') {
    addVariableBindings(statement, bindings);
  }
  if (statement.type === 'FunctionDeclaration' && statement.id) {
    bindings.push({ name: statement.id.name, kind: 'function' });
  }
  if (statement.type === 'ClassDeclaration' && statement.id) {
    bindings.push({ name: statement.id.name, kind: 'class' });
  }
  // var is function scoped, so a var declaration in a top-level block still
  // belongs to this cell. Do not descend into nested functions.
  for (const key of ['consequent', 'alternate', 'body', 'block', 'statement', 'init', 'update']) {
    const child = statement[key];
    if (Array.isArray(child)) {
      for (const item of child) if (item?.type?.endsWith('Statement')) collectStatementBindings(item, bindings);
    } else if (child?.type?.endsWith('Statement')) {
      collectStatementBindings(child, bindings);
    }
  }
}

function collectBindings(ast: any): CellBinding[] {
  const bindings: CellBinding[] = [];
  for (const statement of ast.body ?? []) {
    if (statement.type === 'VariableDeclaration') addVariableBindings(statement, bindings);
    else if (statement.type === 'FunctionDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'function' });
    } else if (statement.type === 'ClassDeclaration' && statement.id) {
      bindings.push({ name: statement.id.name, kind: 'class' });
    } else {
      collectStatementBindings(statement, bindings);
    }
  }
  return bindings;
}

export function analyzeCell(source: string): CellAnalysis {
  let ast: any;
  try {
    ast = parseModule(source, { next: true, ranges: true });
  } catch (error: any) {
    const message = String(error?.message ?? error);
    if (/Illegal return statement|return statement/.test(message)) throw new SyntaxError(TOP_LEVEL_RETURN_ERROR);
    throw error;
  }

  for (const statement of ast.body ?? []) {
    if (statement.type === 'ImportDeclaration' || statement.type.startsWith('Export')) {
      throw new SyntaxError(STATIC_IMPORT_ERROR);
    }
  }

  const last = ast.body?.[ast.body.length - 1];
  const declarations: CellAnalysis['declarations'] = [];
  for (const statement of ast.body ?? []) {
    if (statement.type === 'VariableDeclaration') {
      const names: string[] = [];
      for (const declaration of statement.declarations) collectPatternNames(declaration.id, names);
      declarations.push({ start: statement.start, end: statement.end, names, kind: statement.kind });
    } else if (statement.type === 'FunctionDeclaration' && statement.id) {
      declarations.push({ start: statement.start, end: statement.end, names: [statement.id.name], kind: 'function' });
    } else if (statement.type === 'ClassDeclaration' && statement.id) {
      declarations.push({ start: statement.start, end: statement.end, names: [statement.id.name], kind: 'class' });
    }
  }

  return {
    source,
    bindings: collectBindings(ast),
    declarations,
    finalExpression:
      last?.type === 'ExpressionStatement'
        ? {
            statementStart: last.start,
            statementEnd: last.end,
            expressionStart: last.expression.start,
            expressionEnd: last.expression.end,
          }
        : undefined,
  };
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
  return new SyntaxError(`Identifier '${name}' has already been declared`);
}
