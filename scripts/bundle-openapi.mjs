// Merges the split OpenAPI sources into one document: api/openapi.yaml (generated, committed).
//   openapi/base.yaml            info, servers, tags, security, shared parameters/responses/schemas
//   openapi/paths/*.yaml         each a map of "/path": PathItem
//   openapi/schemas/*.yaml       each a map of SchemaName: Schema
// A path or schema defined twice is an error, so areas can't silently overwrite each other.
import { readFileSync, readdirSync, mkdirSync, writeFileSync } from 'node:fs'
import { parse, stringify } from 'yaml'

const read = (p) => parse(readFileSync(p, 'utf8')) ?? {}
const doc = read('openapi/base.yaml')
doc.paths = {}
doc.components.schemas ??= {}

const merge = (dir, into, what) => {
  for (const f of readdirSync(dir).filter((x) => x.endsWith('.yaml')).sort()) {
    for (const [k, v] of Object.entries(read(`${dir}/${f}`))) {
      if (k in into) throw new Error(`${what} ${k} defined twice (${f})`)
      into[k] = v
    }
  }
}
merge('openapi/paths', doc.paths, 'path')
merge('openapi/schemas', doc.components.schemas, 'schema')

// Stable order: paths by tag area then path; schemas alphabetical.
doc.paths = Object.fromEntries(Object.entries(doc.paths).sort(([a], [b]) => a.localeCompare(b)))
doc.components.schemas = Object.fromEntries(Object.entries(doc.components.schemas).sort(([a], [b]) => a.localeCompare(b)))

mkdirSync('api', { recursive: true })
writeFileSync('api/openapi.yaml', stringify(doc, { lineWidth: 0, aliasDuplicateObjects: false }))

// ── OpenAPI 3.0.3 copy for Go codegen (oapi-codegen / kin-openapi speak 3.0) ──
// Same document, with the 3.1-only constructs rewritten to their 3.0 equivalents. Not a contract: generated.
const to30 = (x) => {
  if (Array.isArray(x)) return x.map(to30)
  if (!x || typeof x !== 'object') return x
  const o = {}
  for (const [k, v] of Object.entries(x)) o[k] = to30(v)
  if (Array.isArray(o.type)) {                       // type: [string, 'null'] -> type: string, nullable
    const t = o.type.filter((t) => t !== 'null')
    if (t.length < o.type.length) o.nullable = true
    if (t.length === 1) o.type = t[0]
    else delete o.type
  }
  for (const key of ['oneOf', 'anyOf']) {            // oneOf: [X, {type: 'null'}] -> allOf: [X], nullable
    if (!o[key]) continue
    const rest = o[key].filter((s) => !(s && s.type === 'null' && Object.keys(s).length === 1))
    if (rest.length < o[key].length) {
      o.nullable = true
      if (rest.length === 1) { delete o[key]; o.allOf = rest } else o[key] = rest
    }
  }
  if ('const' in o) { o.enum = [o.const]; delete o.const }
  if (Array.isArray(o.examples) && !o.content) { o.example = o.examples[0]; delete o.examples }
  for (const [ex, bound] of [['exclusiveMinimum', 'minimum'], ['exclusiveMaximum', 'maximum']]) {
    if (typeof o[ex] === 'number') { o[bound] = o[ex]; o[ex] = true }
  }
  return o
}
const doc30 = to30(doc)
doc30.openapi = '3.0.3'
delete doc30.info.summary
writeFileSync('api/openapi.codegen.yaml', stringify(doc30, { lineWidth: 0, aliasDuplicateObjects: false }))

const ops = Object.values(doc.paths).flatMap((p) => Object.keys(p).filter((m) => ['get', 'post', 'put', 'patch', 'delete'].includes(m)))
console.log(`api/openapi.yaml: ${Object.keys(doc.paths).length} paths, ${ops.length} operations, ${Object.keys(doc.components.schemas).length} schemas`)
