// Collectors index - Re-export all telemetry collectors

export { getErrorCollector, installErrorCollector, uninstallErrorCollector } from './errors'
export { getPerformanceCollector, installPerformanceCollector, uninstallPerformanceCollector } from './performance'
export { getConnectionCollector, installConnectionCollector, uninstallConnectionCollector } from './connection'
