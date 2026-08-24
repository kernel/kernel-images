interface CacheOptions {
  refresh?: boolean;
}

// A CDP page target ID is stable for the lifetime of its Playwright Page.
// Weak keys let closed pages and their IDs be collected without explicit
// eviction; refresh and reset cover target replacement and browser reconnects.
export class PageTargetIdCache<Page extends object> {
  #targetIdMemo = new WeakMap<Page, string>();
  #targetIdInFlight = new WeakMap<Page, Promise<string>>();
  #generation = 0;
  readonly #discoverTargetId: (page: Page) => Promise<string>;

  constructor(discoverTargetId: (page: Page) => Promise<string>) {
    this.#discoverTargetId = discoverTargetId;
  }

  async get(page: Page, options: CacheOptions = {}): Promise<string> {
    if (!options.refresh) {
      const cached = this.#targetIdMemo.get(page);
      if (cached !== undefined) return cached;
    } else {
      this.#targetIdMemo.delete(page);
    }

    const inFlight = this.#targetIdInFlight.get(page);
    if (inFlight !== undefined) return inFlight;

    const generation = this.#generation;
    const discovery = this.#discoverTargetId(page)
      .then(targetId => {
        if (generation === this.#generation) {
          this.#targetIdMemo.set(page, targetId);
        }
        return targetId;
      })
      .finally(() => {
        if (this.#targetIdInFlight.get(page) === discovery) {
          this.#targetIdInFlight.delete(page);
        }
      });

    this.#targetIdInFlight.set(page, discovery);
    return discovery;
  }

  async buildPageByTargetId(pages: readonly Page[], options: CacheOptions = {}): Promise<Map<string, Page>> {
    const pageByTargetId = new Map<string, Page>();

    for (const page of pages) {
      try {
        pageByTargetId.set(await this.get(page, options), page);
      } catch {
        // A crashed or closing page can fail target discovery. Exclude it from
        // this snapshot without preventing other live pages from resolving.
      }
    }

    return pageByTargetId;
  }

  reset(): void {
    this.#targetIdMemo = new WeakMap<Page, string>();
    this.#targetIdInFlight = new WeakMap<Page, Promise<string>>();
    this.#generation++;
  }
}
