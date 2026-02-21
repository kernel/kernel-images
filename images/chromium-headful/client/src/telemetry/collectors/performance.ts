// Performance Collector - Comprehensive performance monitoring including Core Web Vitals

import { getTelemetry } from '../service'
import { PerformanceMetrics } from '../types'

interface PerformanceMemory {
  usedJSHeapSize: number
  totalJSHeapSize: number
  jsHeapSizeLimit: number
}

interface ExtendedPerformance extends Performance {
  memory?: PerformanceMemory
}

interface LayoutShiftEntry extends PerformanceEntry {
  value: number
  hadRecentInput: boolean
}

interface LargestContentfulPaint extends PerformanceEntry {
  renderTime: number
  loadTime: number
  size: number
  element?: Element
}

interface FirstInputEntry extends PerformanceEntry {
  processingStart: number
}

interface PerformanceLongTaskTiming extends PerformanceEntry {
  attribution: TaskAttributionTiming[]
}

interface TaskAttributionTiming {
  containerType: string
  containerSrc: string
  containerId: string
  containerName: string
}

class PerformanceCollector {
  private isInstalled = false
  private fpsMonitorId: number | null = null
  private memoryMonitorId: number | null = null
  private performanceObservers: PerformanceObserver[] = []

  // Core Web Vitals tracking
  private clsValue = 0
  private clsEntries: LayoutShiftEntry[] = []
  private lcpValue = 0
  private fidValue = 0

  // FPS tracking
  private frameCount = 0
  private lastFrameTime = 0
  private fpsValues: number[] = []

  install(): void {
    if (this.isInstalled) {
      return
    }

    this.isInstalled = true

    // Track navigation timing when page loads
    this.trackNavigationTiming()

    // Setup Core Web Vitals observers
    this.setupCoreWebVitals()

    // Setup Long Task observer
    this.setupLongTaskObserver()

    // Start FPS monitoring
    this.startFPSMonitoring()

    // Start memory monitoring
    this.startMemoryMonitoring()
  }

  private trackNavigationTiming(): void {
    // Wait for page to fully load
    if (document.readyState === 'complete') {
      this.captureNavigationMetrics()
    } else {
      window.addEventListener('load', () => {
        // Delay slightly to ensure all metrics are available
        setTimeout(() => this.captureNavigationMetrics(), 100)
      })
    }
  }

  private captureNavigationMetrics(): void {
    const telemetry = getTelemetry()
    const navigation = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming

    if (!navigation) {
      return
    }

    const metrics: PerformanceMetrics = {
      // Navigation timing
      domContentLoaded: navigation.domContentLoadedEventEnd - navigation.startTime,
      domComplete: navigation.domComplete - navigation.startTime,
      loadEventEnd: navigation.loadEventEnd - navigation.startTime,
      ttfb: navigation.responseStart - navigation.requestStart,

      // Resource metrics
      resourceCount: performance.getEntriesByType('resource').length,
      transferSize: navigation.transferSize,
    }

    // Add memory metrics if available
    const extPerf = performance as ExtendedPerformance
    if (extPerf.memory) {
      metrics.usedJSHeapSize = extPerf.memory.usedJSHeapSize
      metrics.totalJSHeapSize = extPerf.memory.totalJSHeapSize
      metrics.jsHeapSizeLimit = extPerf.memory.jsHeapSizeLimit
    }

    telemetry.trackPerformance(metrics, 'perf_page_load')
  }

  private setupCoreWebVitals(): void {
    // Largest Contentful Paint (LCP)
    this.observeLCP()

    // First Input Delay (FID)
    this.observeFID()

    // Cumulative Layout Shift (CLS)
    this.observeCLS()

    // First Contentful Paint (FCP)
    this.observeFCP()
  }

  private observeLCP(): void {
    try {
      const observer = new PerformanceObserver((entryList) => {
        const entries = entryList.getEntries() as LargestContentfulPaint[]
        const lastEntry = entries[entries.length - 1]

        if (lastEntry) {
          this.lcpValue = lastEntry.renderTime || lastEntry.loadTime

          const telemetry = getTelemetry()
          telemetry.trackPerformance(
            {
              lcp: this.lcpValue,
            },
            'perf_largest_contentful_paint',
          )
        }
      })

      observer.observe({ type: 'largest-contentful-paint', buffered: true })
      this.performanceObservers.push(observer)
    } catch (e) {
      // LCP not supported
    }
  }

  private observeFID(): void {
    try {
      const observer = new PerformanceObserver((entryList) => {
        const entries = entryList.getEntries() as FirstInputEntry[]
        const firstEntry = entries[0]

        if (firstEntry) {
          this.fidValue = firstEntry.processingStart - firstEntry.startTime

          const telemetry = getTelemetry()
          telemetry.trackPerformance(
            {
              fid: this.fidValue,
            },
            'perf_first_input_delay',
          )
        }
      })

      observer.observe({ type: 'first-input', buffered: true })
      this.performanceObservers.push(observer)
    } catch (e) {
      // FID not supported
    }
  }

  private observeCLS(): void {
    try {
      const observer = new PerformanceObserver((entryList) => {
        const entries = entryList.getEntries() as LayoutShiftEntry[]

        for (const entry of entries) {
          // Only count layout shifts without recent input
          if (!entry.hadRecentInput) {
            this.clsValue += entry.value
            this.clsEntries.push(entry)
          }
        }
      })

      observer.observe({ type: 'layout-shift', buffered: true })
      this.performanceObservers.push(observer)

      // Report CLS when page becomes hidden
      document.addEventListener('visibilitychange', () => {
        if (document.visibilityState === 'hidden' && this.clsValue > 0) {
          const telemetry = getTelemetry()
          telemetry.trackPerformance(
            {
              cls: this.clsValue,
            },
            'perf_cumulative_layout_shift',
          )
        }
      })
    } catch (e) {
      // CLS not supported
    }
  }

  private observeFCP(): void {
    try {
      const observer = new PerformanceObserver((entryList) => {
        const entries = entryList.getEntries()
        const fcpEntry = entries.find((entry) => entry.name === 'first-contentful-paint')

        if (fcpEntry) {
          const telemetry = getTelemetry()
          telemetry.trackPerformance(
            {
              fcp: fcpEntry.startTime,
            },
            'perf_first_contentful_paint',
          )
        }
      })

      observer.observe({ type: 'paint', buffered: true })
      this.performanceObservers.push(observer)
    } catch (e) {
      // FCP not supported
    }
  }

  private setupLongTaskObserver(): void {
    try {
      const observer = new PerformanceObserver((entryList) => {
        const entries = entryList.getEntries() as PerformanceLongTaskTiming[]
        const telemetry = getTelemetry()

        for (const entry of entries) {
          // Track long tasks (>50ms)
          if (entry.duration > 50) {
            const attribution = entry.attribution?.[0]
            telemetry.track(
              'perf_long_task',
              entry.duration > 200 ? 'warning' : 'info',
              {
                duration: entry.duration,
                startTime: entry.startTime,
                containerType: attribution?.containerType,
                containerSrc: attribution?.containerSrc,
                containerId: attribution?.containerId,
                containerName: attribution?.containerName,
              },
            )
          }
        }
      })

      observer.observe({ type: 'longtask', buffered: true })
      this.performanceObservers.push(observer)
    } catch (e) {
      // Long task observer not supported
    }
  }

  private startFPSMonitoring(): void {
    const measureFPS = (timestamp: number) => {
      if (this.lastFrameTime > 0) {
        const delta = timestamp - this.lastFrameTime
        if (delta > 0) {
          const fps = 1000 / delta
          this.fpsValues.push(fps)

          // Keep only last 60 samples (1 second at 60fps)
          if (this.fpsValues.length > 60) {
            this.fpsValues.shift()
          }
        }
      }

      this.lastFrameTime = timestamp
      this.frameCount++
      this.fpsMonitorId = requestAnimationFrame(measureFPS)
    }

    this.fpsMonitorId = requestAnimationFrame(measureFPS)

    // Report FPS every 10 seconds
    setInterval(() => {
      if (this.fpsValues.length > 0) {
        const avgFps = this.fpsValues.reduce((a, b) => a + b, 0) / this.fpsValues.length
        const minFps = Math.min(...this.fpsValues)
        const maxFps = Math.max(...this.fpsValues)

        // Calculate dropped frames (assuming 60fps target)
        const droppedFrames = this.fpsValues.filter((fps) => fps < 55).length

        const telemetry = getTelemetry()
        telemetry.trackPerformance(
          {
            fps: Math.round(avgFps),
            droppedFrames,
          },
          'perf_fps',
        )

        // Also track as detailed data
        telemetry.track('perf_fps', avgFps < 30 ? 'warning' : 'debug', {
          avgFps: Math.round(avgFps),
          minFps: Math.round(minFps),
          maxFps: Math.round(maxFps),
          droppedFrames,
          sampleCount: this.fpsValues.length,
        })
      }
    }, 10000)
  }

  private startMemoryMonitoring(): void {
    // Only available in Chrome
    const extPerf = performance as ExtendedPerformance
    if (!extPerf.memory) {
      return
    }

    // Report memory every 30 seconds
    this.memoryMonitorId = window.setInterval(() => {
      const memory = extPerf.memory
      if (memory) {
        const telemetry = getTelemetry()

        const usedMB = Math.round(memory.usedJSHeapSize / (1024 * 1024))
        const totalMB = Math.round(memory.totalJSHeapSize / (1024 * 1024))
        const limitMB = Math.round(memory.jsHeapSizeLimit / (1024 * 1024))
        const usagePercent = Math.round((memory.usedJSHeapSize / memory.jsHeapSizeLimit) * 100)

        // Warn if memory usage is high
        const severity = usagePercent > 80 ? 'warning' : usagePercent > 90 ? 'error' : 'debug'

        telemetry.track('perf_memory', severity, {
          usedJSHeapSize: memory.usedJSHeapSize,
          totalJSHeapSize: memory.totalJSHeapSize,
          jsHeapSizeLimit: memory.jsHeapSizeLimit,
          usedMB,
          totalMB,
          limitMB,
          usagePercent,
        })
      }
    }, 30000)
  }

  // Public method to get current metrics
  getCurrentMetrics(): PerformanceMetrics {
    const extPerf = performance as ExtendedPerformance
    const avgFps = this.fpsValues.length > 0 ? this.fpsValues.reduce((a, b) => a + b, 0) / this.fpsValues.length : 0

    const metrics: PerformanceMetrics = {
      fps: Math.round(avgFps),
      droppedFrames: this.fpsValues.filter((fps) => fps < 55).length,
      lcp: this.lcpValue,
      fid: this.fidValue,
      cls: this.clsValue,
    }

    if (extPerf.memory) {
      metrics.usedJSHeapSize = extPerf.memory.usedJSHeapSize
      metrics.totalJSHeapSize = extPerf.memory.totalJSHeapSize
      metrics.jsHeapSizeLimit = extPerf.memory.jsHeapSizeLimit
    }

    return metrics
  }

  uninstall(): void {
    if (!this.isInstalled) {
      return
    }

    // Stop FPS monitoring
    if (this.fpsMonitorId !== null) {
      cancelAnimationFrame(this.fpsMonitorId)
      this.fpsMonitorId = null
    }

    // Stop memory monitoring
    if (this.memoryMonitorId !== null) {
      clearInterval(this.memoryMonitorId)
      this.memoryMonitorId = null
    }

    // Disconnect all performance observers
    for (const observer of this.performanceObservers) {
      observer.disconnect()
    }
    this.performanceObservers = []

    this.isInstalled = false
  }
}

// Singleton instance
let performanceCollectorInstance: PerformanceCollector | null = null

export function getPerformanceCollector(): PerformanceCollector {
  if (!performanceCollectorInstance) {
    performanceCollectorInstance = new PerformanceCollector()
  }
  return performanceCollectorInstance
}

export function installPerformanceCollector(): void {
  getPerformanceCollector().install()
}

export function uninstallPerformanceCollector(): void {
  if (performanceCollectorInstance) {
    performanceCollectorInstance.uninstall()
  }
}
