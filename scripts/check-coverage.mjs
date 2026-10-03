// Compares the spec's operations with the frontend mock's endpoint list (docs/api-contract.md in the frontend repo).
// Usage: node scripts/check-coverage.mjs [path/to/api-contract.md]
import { readFileSync } from 'node:fs'
import { parse } from 'yaml'

const file = process.argv[2] ?? '../ecopurnity/docs/api-contract.md'
const mock = new Set(
  [...readFileSync(file, 'utf8').matchAll(/^\| (GET|POST|PUT|PATCH|DELETE) \| `([^`]+)` \|$/gm)].map(
    (m) => `${m[1]} ${m[2].replace(/^\/api\/v1/, '').replace(/:(\w+)/g, '{$1}')}`,
  ),
)
const doc = parse(readFileSync('api/openapi.yaml', 'utf8'))
const spec = new Set(
  Object.entries(doc.paths).flatMap(([p, item]) => Object.keys(item).filter((m) => ['get', 'post', 'put', 'patch', 'delete'].includes(m)).map((m) => `${m.toUpperCase()} ${p}`)),
)
// {orgId} vs :orgId param names may differ in spelling between mock and spec; compare by shape.
const shape = (s) => s.replace(/\{[^}]+\}/g, '{}')
const bySh = (set) => new Map([...set].map((s) => [shape(s), s]))
const m = bySh(mock), s = bySh(spec)
const missing = [...m.keys()].filter((k) => !s.has(k)).map((k) => m.get(k))
const extra = [...s.keys()].filter((k) => !m.has(k)).map((k) => s.get(k))
console.log(`mock ${mock.size} · spec ${spec.size} · missing in spec ${missing.length} · not in mock ${extra.length}`)
if (missing.length) console.log('MISSING:\n  ' + missing.join('\n  '))
if (extra.length) console.log('EXTRA:\n  ' + extra.join('\n  '))
process.exit(missing.length || extra.length ? 1 : 0)
