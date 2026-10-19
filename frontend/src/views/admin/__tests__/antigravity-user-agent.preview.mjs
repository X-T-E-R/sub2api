// Local-only visual fixture. All API requests are mocked; no backend is contacted.
// Run from frontend: node src/views/admin/__tests__/antigravity-user-agent.preview.mjs
import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

const user = { id: 1, role: 'admin', email: 'ua-preview@example.test', username: 'UA preview', balance: 0, concurrency: 1, status: 'active' }
let settings = {
  site_name: 'Sub2API', backend_mode_enabled: false,
  table_default_page_size: 20, table_page_size_options: [10, 20, 50, 100],
  default_subscriptions: [], default_platform_quotas: {}, custom_menu_items: [], custom_endpoints: [],
  registration_email_suffix_whitelist: [], forwarded_client_ip_headers: ['X-Forwarded-For'],
  payment_load_balance_strategy: 'round-robin', payment_enabled_types: [],
  antigravity_user_agent: 'antigravity/hub/2.23.0 darwin/arm64', antigravity_user_agent_version: '1.23.2',
  passkey_configured: false, step_up_enabled: false,
}
const evidence = { saves: [], rejectNextSave: false }
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  resolve: { alias: { '@': resolve('src'), 'vue-i18n': 'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js' } },
  define: { __INTLIFY_JIT_COMPILATION__: true },
  server: { host: '127.0.0.1', port: 43127, strictPort: true },
  plugins: [vue(), {
    name: 'antigravity-user-agent-preview',
    transformIndexHtml(html) {
      const bootstrap = `localStorage.setItem('auth_token','local-preview-not-a-real-token');localStorage.setItem('auth_user',${JSON.stringify(JSON.stringify(user))});localStorage.setItem('locale','zh-CN');window.__APP_CONFIG__=${JSON.stringify(settings)};`
      return html.replace('</head>', `<script>${bootstrap}</script></head>`)
    },
    configureServer(vite) {
      vite.middlewares.use(async (req, res, next) => {
        const path = new URL(req.url, 'http://127.0.0.1:43127').pathname
        if (!path.startsWith('/api/') && !path.startsWith('/__ua-test__/')) return next()
        const reply = (data, status = 200) => {
          res.statusCode = status
          res.setHeader('Content-Type', 'application/json')
          res.end(JSON.stringify(data))
        }
        if (path === '/__ua-test__/evidence') return reply(evidence)
        if (path === '/__ua-test__/reject-next-save') {
          evidence.rejectNextSave = true
          return reply({ armed: true })
        }
        let data = {}
        if (path === '/api/v1/admin/settings') {
          if (req.method === 'PUT') {
            let body = ''
            for await (const chunk of req) body += chunk
            const payload = JSON.parse(body)
            evidence.saves.push(payload)
            if (evidence.rejectNextSave) {
              evidence.rejectNextSave = false
              return reply({ code: 400, message: 'Antigravity User-Agent could not be saved. Try again.' }, 400)
            }
            settings = { ...settings, ...payload, antigravity_user_agent: payload.antigravity_user_agent.trim() }
          }
          data = settings
        } else if (path.endsWith('/settings/public')) data = settings
        else if (path.endsWith('/auth/me')) data = user
        else if (path.includes('/setup')) data = { needs_setup: false }
        else if (path.includes('groups') || path.includes('providers') || path.includes('announcements')) data = []
        else if (path.includes('web-search')) data = { enabled: false, providers: [] }
        else if (path.includes('antigravity-model-effort')) data = { levels: ['low', 'medium', 'high'], default_effort: 'high' }
        else if (path.includes('antigravity-thought-policy')) data = { suppress_thought_only_stop: false, continue_thought_only: false }
        else if (path.includes('model-reasoning-floor') || path.includes('beta-policy')) data = { rules: [] }
        else if (path.includes('model-test')) data = { prompt: '', builtin_default: 'Hello' }
        else if (path.includes('email-templates')) data = { events: [], locales: [], placeholders: [] }
        else if (path.includes('/backups/image-storage')) data = { config: { prefix: 'images/', region: 'auto' }, secret_configured: false }
        else if (path.includes('compliance')) data = { accepted: true, required: false }
        else data = { enabled: false, exists: false, rules: [], items: [], data: [], total: 0, group_ids: [], cooldown_seconds: 5, cooldown_minutes: 10, interval_minutes: 30 }
        return reply({ code: 0, data })
      })
    },
  }],
})
await server.listen()
console.log('Mock SettingsView: http://127.0.0.1:43127/admin/settings')
