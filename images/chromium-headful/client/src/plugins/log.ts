import { PluginObject } from 'vue'

interface Logger {
  error(error: Error): void
  warn(...log: any[]): void
  info(...log: any[]): void
  debug(...log: any[]): void
}

declare global {
  interface Window {
    $log: Logger
  }
}

declare module 'vue/types/vue' {
  interface Vue {
    $log: Logger
  }
}

const noop = () => {}
const noopError = (_: Error) => {}

const realLoggers: Logger = {
  error: (error: Error) => console.error('[%cNEKO%c] %cERR', 'color: #498ad8;', '', 'color: #d84949;', error),
  warn: (...log: any[]) => console.warn('[%cNEKO%c] %cWRN', 'color: #498ad8;', '', 'color: #eae364;', ...log),
  info: (...log: any[]) => console.info('[%cNEKO%c] %cINF', 'color: #498ad8;', '', 'color: #4ac94c;', ...log),
  debug: (...log: any[]) => console.log('[%cNEKO%c] %cDBG', 'color: #498ad8;', '', 'color: #eae364;', ...log),
}

const offLoggers: Logger = {
  error: noopError,
  warn: noop,
  info: noop,
  debug: noop,
}

const LOG_METHODS: (keyof Logger)[] = ['error', 'warn', 'info', 'debug']
const DISABLED_LEVELS = ['off', 'none', '']

function createLoggerForLevel(level: string): Logger {
  const normalized = level.toLowerCase()
  if (DISABLED_LEVELS.includes(normalized)) return offLoggers

  const enabledIndex = LOG_METHODS.indexOf(normalized as keyof Logger)
  if (enabledIndex === -1) return offLoggers

  const logger: Logger = { ...offLoggers }
  for (let i = 0; i <= enabledIndex; i++) {
    const method = LOG_METHODS[i]
    ;(logger as any)[method] = realLoggers[method]
  }
  return logger
}

function getLogLevel(): string {
  const params = new URL(location.href).searchParams
  return params.get('log_level') ?? params.get('logLevel') ?? 'off'
}

const plugin: PluginObject<undefined> = {
  install(Vue) {
    window.$log = createLoggerForLevel(getLogLevel())
    Vue.prototype.$log = window.$log
  },
}

export default plugin
