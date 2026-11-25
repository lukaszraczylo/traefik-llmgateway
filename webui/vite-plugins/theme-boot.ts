import { createHash } from 'node:crypto'

import { themeBootScript } from '@perfect-design/ui/stores/theme-boot'
import type { Plugin } from 'vite'

/** Ships perfect-design's pre-paint theme script as an external file: the admin CSP (script-src 'self') forbids the inline form. */
export function themeBoot(): Plugin {
  const hash = createHash('sha256').update(themeBootScript).digest('hex').slice(0, 8)
  const fileName = `assets/theme-boot.${hash}.js`
  let base = '/'

  return {
    name: 'theme-boot',
    configResolved(config) {
      base = config.base
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (req.url?.split('?')[0] !== `${base}${fileName}`) return next()
        res.setHeader('Content-Type', 'text/javascript; charset=utf-8')
        res.end(themeBootScript)
      })
    },
    generateBundle() {
      this.emitFile({ type: 'asset', fileName, source: themeBootScript })
    },
    transformIndexHtml() {
      return [{ tag: 'script', attrs: { src: `${base}${fileName}` }, injectTo: 'head-prepend' }]
    },
  }
}
