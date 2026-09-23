import type { BrowserReplCdpClient, CdpEvent, CdpTarget } from './browser-cdp-client';
import { CustomToolDefinitions, matchesURLPattern, normalizedURL } from './custom-webmcp-definitions.ts';
import type { AddCustomToolsInput, CustomToolDefinition, CustomToolFrameMatch, CustomToolSummary } from './custom-webmcp-definitions.ts';
import { BINDING_NAME, CustomWebMCPPageRuntime, errorMessage, MAIN_PAGE_RUNTIME_KEY, PAGE_RUNTIME_KEY } from './custom-webmcp-page-runtime.ts';

export { matchesURLPattern } from './custom-webmcp-definitions.ts';

const WORLD_NAME = 'kernel-custom-webmcp';
const RECONNECT_DELAY_MS = 250;
const RECONCILE_EVENT_DELAY_MS = 50;
const RECONCILE_COMMAND_TIMEOUT_MS = 5_000;

interface CdpFrameTree {
  frame: {
    id: string;
    loaderId?: string;
    url?: string;
  };
  childFrames?: CdpFrameTree[];
}

interface FrameInfo {
  id: string;
  sessionId: string;
  targetId: string | null;
  url: string;
  children?: FrameInfo[];
}

interface IframeSession {
  targetId: string;
  sessionId: string;
}

interface PageState {
  targetId: string;
  sessionId: string;
  documentKey: string;
  contextDocumentKey: string;
  contextId: number | null;
  signature: string;
  matches: Map<string, CustomToolFrameMatch[]>;
  errors: string[];
}

class CustomToolNotFoundError extends Error {
  readonly code = 'custom_tool_not_found';
}

function flattenFrames(frame: FrameInfo): FrameInfo[] {
  const frames = [frame];
  for (const child of frame.children ?? []) frames.push(...flattenFrames(child));
  return frames;
}

export class CustomWebMCPRegistry {
  private readonly definitions: CustomToolDefinitions;
  private readonly pageRuntime: CustomWebMCPPageRuntime;
  private emptyRegistryCleaned = false;
  private pages = new Map<string, PageState>();
  private sessions = new Map<string, string>();
  private iframeSessions = new Map<string, IframeSession>();
  private iframeTargetsBySession = new Map<string, string>();
  private reconciliation: Promise<void> | null = null;
  private reconcileAgain = false;
  private reconcileTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private connectionGeneration = 0;
  private disposed = false;
  private readonly unsubscribeEvent: () => void;
  private readonly unsubscribeDisconnect: () => void;
  private onError?: (message: string) => void;
  private readonly client: BrowserReplCdpClient;

  constructor(
    client: BrowserReplCdpClient,
    runInvocation: <T>(signal: AbortSignal, callback: () => Promise<T>) => Promise<T>,
    publishDefinitions: (tools: CustomToolSummary[]) => void,
  ) {
    this.client = client;
    this.definitions = new CustomToolDefinitions(publishDefinitions);
    this.pageRuntime = new CustomWebMCPPageRuntime(client, runInvocation, (sessionId, id, revision) => {
      const targetId = this.sessions.get(sessionId);
      const page = targetId ? this.pages.get(targetId) : undefined;
      const definition = this.definitions.get(id);
      if (!page || !definition || definition.revision !== revision) return undefined;
      return {definition, matches: page.matches.get(id) ?? []};
    });
    this.unsubscribeEvent = client.subscribeEvents((event) => this.handleEvent(event));
    this.unsubscribeDisconnect = client.subscribeDisconnect(() => this.handleDisconnect());
    this.scheduleReconcile();
  }

  setErrorHandler(handler: (message: string) => void): void {
    this.onError = handler;
  }

  add = async (input: AddCustomToolsInput): Promise<CustomToolSummary[]> => {
    const added = await this.definitions.add(input);
    this.scheduleReconcile();
    await this.settleReconciliation();
    return added;
  };

  remove = async (id: string): Promise<boolean> => {
    if (!this.definitions.remove(id)) return false;
    if (this.definitions.size === 0) this.emptyRegistryCleaned = false;
    this.scheduleReconcile();
    await this.settleReconciliation();
    return true;
  };

  list = (): CustomToolSummary[] => this.definitions.list();
  isCDP = (id: string): boolean => this.definitions.isCDP(id);
  hasCDP = (): boolean => this.definitions.hasCDP();

  invokeCDP = async (id: string, targetId: string, input: Record<string, unknown>, signal?: AbortSignal): Promise<unknown> => {
    await this.reconcile();
    const definition = this.definitions.get(id);
    const matches = this.pages.get(targetId)?.matches.get(id);
    if (definition?.kind !== 'cdp' || !matches?.length) {
      throw new CustomToolNotFoundError('custom tool is no longer available; discover tools again');
    }
    return this.pageRuntime.invokeCDP(definition, matches, input, signal);
  };

  private async settleReconciliation(): Promise<void> {
    try {
      await this.reconcile();
    } catch (error) {
      this.reportError(error);
      this.retryReconcile();
    }
  }

  async dispose(): Promise<void> {
    this.disposed = true;
    this.unsubscribeEvent();
    this.unsubscribeDisconnect();
    if (this.reconcileTimer) clearTimeout(this.reconcileTimer);
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    if (this.reconciliation) {
      await Promise.race([
        this.reconciliation.catch(() => undefined),
        new Promise((resolve) => setTimeout(resolve, 500)),
      ]);
    }
    this.pageRuntime.abortAll();
    await Promise.all([...this.pages.values()].map(async (page) => {
      await this.pageRuntime.dispose(page.sessionId, MAIN_PAGE_RUNTIME_KEY);
      if (page.contextId !== null) await this.pageRuntime.dispose(page.sessionId, PAGE_RUNTIME_KEY, page.contextId);
      await this.detachPage(page);
    }));
    await Promise.all([...this.iframeSessions.values()].map((frame) => this.detachSession(frame.sessionId)));
    this.pages.clear();
    this.sessions.clear();
    this.iframeSessions.clear();
    this.iframeTargetsBySession.clear();
  }

  private handleEvent(event: CdpEvent): void {
    if (event.method === 'Runtime.bindingCalled') {
      void this.pageRuntime.handleBinding(event);
      return;
    }
    if (event.method === 'Target.detachedFromTarget') {
      const sessionId = (event.params as {sessionId?: string})?.sessionId;
      if (!sessionId) return;
      const targetId = this.sessions.get(sessionId);
      if (targetId) {
        this.sessions.delete(sessionId);
        this.pages.delete(targetId);
      } else {
        const iframeTargetId = this.iframeTargetsBySession.get(sessionId);
        if (!iframeTargetId) return;
        this.iframeTargetsBySession.delete(sessionId);
        this.iframeSessions.delete(iframeTargetId);
      }
    }
    if (
      event.method === 'Target.targetCreated' ||
      event.method === 'Target.targetInfoChanged' ||
      event.method === 'Target.targetDestroyed' ||
      event.method === 'Target.detachedFromTarget' ||
      event.method === 'Page.frameAttached' ||
      event.method === 'Page.frameNavigated' ||
      event.method === 'Page.frameDetached'
    ) {
      this.scheduleReconcile();
    }
  }

  private handleDisconnect(): void {
    this.connectionGeneration++;
    this.emptyRegistryCleaned = false;
    this.pages.clear();
    this.sessions.clear();
    this.iframeSessions.clear();
    this.iframeTargetsBySession.clear();
    if (this.disposed || this.reconnectTimer) return;
    this.retryReconcile();
  }

  private scheduleReconcile(): void {
    if (this.disposed || this.reconcileTimer) return;
    this.reconcileTimer = setTimeout(() => {
      this.reconcileTimer = null;
      void this.reconcile().catch((error) => {
        this.reportError(error);
        this.retryReconcile();
      });
    }, RECONCILE_EVENT_DELAY_MS);
    this.reconcileTimer.unref?.();
  }

  private retryReconcile(): void {
    if (this.disposed || this.reconnectTimer) return;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.scheduleReconcile();
    }, RECONNECT_DELAY_MS);
    this.reconnectTimer.unref?.();
  }

  private async reconcile(): Promise<void> {
    if (this.disposed) return;
    if (this.reconcileTimer) {
      clearTimeout(this.reconcileTimer);
      this.reconcileTimer = null;
    }
    if (this.reconciliation) {
      this.reconcileAgain = true;
      return this.reconciliation;
    }
    this.reconciliation = (async () => {
      do {
        this.reconcileAgain = false;
        await this.reconcileOnce();
      } while (this.reconcileAgain && !this.disposed);
    })();
    try {
      await this.reconciliation;
    } finally {
      this.reconciliation = null;
    }
  }

  private async reconcileOnce(): Promise<void> {
    if (this.definitions.size === 0 && this.emptyRegistryCleaned) return;
    await this.client.ensureConnected();
    await this.client.send(
      'Target.setDiscoverTargets',
      {discover: true},
      undefined,
      RECONCILE_COMMAND_TIMEOUT_MS,
    );
    const targets = await this.client.listTargets(RECONCILE_COMMAND_TIMEOUT_MS);
    const pages = targets.filter(
      (target) => target.type === 'page' && (target.url.startsWith('https://') || target.url.startsWith('http://')),
    );
    const liveTargets = new Set(pages.map((target) => target.targetId));
    const liveIframeTargets = new Set(
      targets.filter((target) => target.type === 'iframe').map((target) => target.targetId),
    );

    for (const [targetId, page] of this.pages) {
      if (!liveTargets.has(targetId)) {
        await this.detachPage(page);
        this.pages.delete(targetId);
        this.sessions.delete(page.sessionId);
      }
    }
    for (const [targetId, frame] of this.iframeSessions) {
      if (!liveIframeTargets.has(targetId)) {
        await this.detachSession(frame.sessionId);
        this.iframeSessions.delete(targetId);
        this.iframeTargetsBySession.delete(frame.sessionId);
      }
    }

    await Promise.all(pages.map(async (target) => {
      try {
        await this.reconcilePage(target, targets);
      } catch (error) {
        const page = this.pages.get(target.targetId);
        if (page) {
          const message = errorMessage(error);
          if (page.errors[0] !== message) this.reportError(error);
          page.errors = [message];
          page.signature = '';
          if (message.includes('Cannot find context') || message.includes('Session with given id not found')) {
            page.contextId = null;
            page.contextDocumentKey = '';
          }
        } else {
          this.reportError(error);
        }
        this.retryReconcile();
      }
    }));

    if (this.definitions.size === 0) {
      const cleanupGeneration = this.connectionGeneration;
      const pageCleanup = await Promise.all([...this.pages.values()].map(async (page) => {
        const mainDisposed = await this.pageRuntime.dispose(page.sessionId, MAIN_PAGE_RUNTIME_KEY);
        const isolatedDisposed = page.contextId === null ||
          await this.pageRuntime.dispose(page.sessionId, PAGE_RUNTIME_KEY, page.contextId);
        const detached = await this.detachPage(page);
        return mainDisposed && isolatedDisposed && detached;
      }));
      const frameCleanup = await Promise.all(
        [...this.iframeSessions.values()].map((frame) => this.detachSession(frame.sessionId)),
      );
      this.pages.clear();
      this.sessions.clear();
      this.iframeSessions.clear();
      this.iframeTargetsBySession.clear();
      this.emptyRegistryCleaned = cleanupGeneration === this.connectionGeneration &&
        pageCleanup.every(Boolean) && frameCleanup.every(Boolean);
      if (!this.emptyRegistryCleaned) this.retryReconcile();
    }
  }

  private async reconcilePage(target: CdpTarget, targets: CdpTarget[]): Promise<void> {
    let page = this.pages.get(target.targetId);
    if (!page) {
      const attached = await this.client.send<{sessionId: string}>(
        'Target.attachToTarget',
        {targetId: target.targetId, flatten: true},
        undefined,
        RECONCILE_COMMAND_TIMEOUT_MS,
      );
      if (this.disposed) {
        await this.detachSession(attached.sessionId);
        return;
      }
      page = {
        targetId: target.targetId,
        sessionId: attached.sessionId,
        documentKey: '',
        contextDocumentKey: '',
        contextId: null,
        signature: '',
        matches: new Map(),
        errors: [],
      };
      this.pages.set(target.targetId, page);
      this.sessions.set(page.sessionId, target.targetId);
      await this.client.send('Page.enable', undefined, page.sessionId, RECONCILE_COMMAND_TIMEOUT_MS);
      await this.client.send('Runtime.enable', undefined, page.sessionId, RECONCILE_COMMAND_TIMEOUT_MS);
      if (this.disposed) {
        this.pages.delete(target.targetId);
        this.sessions.delete(page.sessionId);
        await this.detachPage(page);
        return;
      }
    }

    const result = await this.client.send<{frameTree: CdpFrameTree}>(
      'Page.getFrameTree',
      undefined,
      page.sessionId,
      RECONCILE_COMMAND_TIMEOUT_MS,
    );
    const root = this.decodeFrameTree(result.frameTree, page.sessionId, target.targetId);
    const framesByID = new Map(flattenFrames(root).map((frame) => [frame.id, frame]));
    const unresolvedIframes = new Map(
      targets
        .filter((candidate) => candidate.type === 'iframe')
        .map((candidate) => [candidate.targetId, candidate]),
    );
    while (unresolvedIframes.size > 0) {
      const children = [...unresolvedIframes.values()].filter(
        (iframe) => iframe.parentFrameId && framesByID.has(iframe.parentFrameId),
      );
      if (children.length === 0) break;
      const childTrees = await Promise.all(children.map(async (iframe) => {
        unresolvedIframes.delete(iframe.targetId);
        const session = await this.ensureIframeSession(iframe.targetId);
        const result = await this.client.send<{frameTree: CdpFrameTree}>(
          'Page.getFrameTree',
          undefined,
          session.sessionId,
          RECONCILE_COMMAND_TIMEOUT_MS,
        );
        return flattenFrames(this.decodeFrameTree(result.frameTree, session.sessionId, iframe.targetId));
      }));
      for (const frame of childTrees.flat()) framesByID.set(frame.id, frame);
    }
    const frames = [...framesByID.values()];
    const matches = new Map<string, CustomToolFrameMatch[]>();
    for (const definition of this.definitions.values()) {
      const matchingFrames = frames
        .filter((frame) => definition.match.url_patterns.some((pattern) => matchesURLPattern(frame.url, pattern)))
        .map((frame) => ({
          frame_id: frame.id,
          session_id: frame.sessionId,
          target_id: frame.targetId,
          top_target_id: target.targetId,
          url: normalizedURL(frame.url),
        }));
      if (matchingFrames.length) matches.set(definition.id, matchingFrames);
    }

    const documentKey = `${root.id}:${result.frameTree.frame.loaderId ?? ''}`;
    const signature = JSON.stringify([...matches.keys()].sort().map((id) => [id, this.definitions.get(id)!.revision]));
    const documentChanged = page.documentKey !== documentKey;
    const contextChanged = page.contextDocumentKey !== documentKey;
    const registrationCurrent = !documentChanged && page.signature === signature && page.contextId !== null;
    page.matches = matches;
    if (registrationCurrent || this.disposed) return;

    if (contextChanged || page.contextId === null) {
      page.contextId = null;
      const world = await this.client.send<{executionContextId: number}>(
        'Page.createIsolatedWorld',
        {frameId: root.id, worldName: WORLD_NAME, grantUniveralAccess: false},
        page.sessionId,
        RECONCILE_COMMAND_TIMEOUT_MS,
      );
      await this.client.send(
        'Runtime.addBinding',
        {name: BINDING_NAME, executionContextName: WORLD_NAME},
        page.sessionId,
        RECONCILE_COMMAND_TIMEOUT_MS,
      );
      page.contextId = world.executionContextId;
      page.contextDocumentKey = documentKey;
    }
    if (this.disposed) return;
    const isolatedContextId = page.contextId;
    if (isolatedContextId === null) throw new Error('custom WebMCP isolated world is unavailable');

    const definitions = [...page.matches.keys()].map((id) => this.definitions.get(id)!);
    const pageErrors = await this.pageRuntime.install(
      page.sessionId,
      definitions.filter((definition) => definition.kind === 'page'),
      MAIN_PAGE_RUNTIME_KEY,
    );
    const cdpErrors = await this.pageRuntime.install(
      page.sessionId,
      definitions.filter((definition) => definition.kind === 'cdp'),
      PAGE_RUNTIME_KEY,
      isolatedContextId,
    );
    const errors = [...pageErrors, ...cdpErrors];
    const changed = errors.join('; ') !== page.errors.join('; ');
    page.errors = errors;
    if (errors.length) {
      page.signature = '';
      if (changed) this.reportError(new Error(`custom WebMCP registration failed: ${errors.join('; ')}`));
      this.retryReconcile();
      return;
    }
    page.documentKey = documentKey;
    page.signature = signature;
  }

  private decodeFrameTree(tree: CdpFrameTree, sessionId: string, targetId: string | null): FrameInfo {
    return {
      id: tree.frame.id,
      sessionId,
      targetId,
      url: tree.frame.url ?? '',
      children: (tree.childFrames ?? []).map((child) => this.decodeFrameTree(child, sessionId, null)),
    };
  }

  private async ensureIframeSession(targetId: string): Promise<IframeSession> {
    const current = this.iframeSessions.get(targetId);
    if (current) return current;
    const attached = await this.client.send<{sessionId: string}>(
      'Target.attachToTarget',
      {targetId, flatten: true},
      undefined,
      RECONCILE_COMMAND_TIMEOUT_MS,
    );
    const session = {targetId, sessionId: attached.sessionId};
    this.iframeSessions.set(targetId, session);
    this.iframeTargetsBySession.set(session.sessionId, targetId);
    await this.client.send('Page.enable', undefined, session.sessionId, RECONCILE_COMMAND_TIMEOUT_MS);
    return session;
  }

  private async detachPage(page: PageState): Promise<boolean> {
    return this.detachSession(page.sessionId);
  }

  private async detachSession(sessionId: string): Promise<boolean> {
    try {
      await this.client.send(
        'Target.detachFromTarget',
        {sessionId},
        undefined,
        1_000,
      );
      return true;
    } catch (error) {
      const message = errorMessage(error);
      return message.includes('No target with given id') ||
        message.includes('Session with given id not found');
    }
  }

  private reportError(error: unknown): void {
    this.onError?.(error instanceof Error ? error.message : String(error));
  }
}
