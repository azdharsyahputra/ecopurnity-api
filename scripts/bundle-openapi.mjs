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
const ops = Object.values(doc.paths).flatMap((p) => Object.keys(p).filter((m) => ['get', 'post', 'put', 'patch', 'delete'].includes(m)))
console.log(`api/openapi.yaml: ${Object.keys(doc.paths).length} paths, ${ops.length} operations, ${Object.keys(doc.components.schemas).length} schemas`)
