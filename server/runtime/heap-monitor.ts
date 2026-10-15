// Logs when the V8 heap nears its limit. The process cannot log once V8 hits
// the limit (it aborts), so the API watches stderr for that case; this covers
// the warning beforehand.

export interface HeapSample {
  usedBytes: number;
  limitBytes: number;
}

export const HEAP_WARN_RATIO = 0.9;
export const HEAP_REARM_RATIO = 0.8;

// Warns once when usage crosses 90% of the limit and again only after usage
// has dropped below 80%, so usage hovering near 90% does not flood the log.
export class HeapMonitor {
  #warned = false;
  readonly #sample: () => HeapSample;
  readonly #warn: (sample: HeapSample) => void;

  constructor(sample: () => HeapSample, warn: (sample: HeapSample) => void) {
    this.#sample = sample;
    this.#warn = warn;
  }

  check(): void {
    const sample = this.#sample();
    const ratio = sample.usedBytes / sample.limitBytes;
    if (!this.#warned && ratio >= HEAP_WARN_RATIO) {
      this.#warned = true;
      this.#warn(sample);
    } else if (this.#warned && ratio < HEAP_REARM_RATIO) {
      this.#warned = false;
    }
  }
}
