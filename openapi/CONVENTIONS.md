# Writing the OpenAPI sources

The source of truth for behaviour is the frontend's mock backend in `/Users/csadeveloper/kkn/ecp/ecopurnity`
(`src/mocks/*.ts`, handlers registered with `http.get(api('/path'), ...)`). Types are in `src/domain/*.ts`,
`src/features/*/types.ts` and `src/features/*/hooks.ts`. Read the handler, not just the path: validation rules,
status codes, error codes, role checks and side effects are all in there and must show up in the spec.

## Layout

```
openapi/base.yaml            info, tags, shared parameters/responses, ApiError, PageMeta   (do not edit)
openapi/paths/<area>.yaml    a map of "/path": PathItem                                      (one file per path agent)
openapi/schemas/<file>.yaml  a map of SchemaName: Schema                                    (one file per schema agent)
```

`npm run openapi:lint` merges everything into `api/openapi.yaml` and lints it. A path or schema name defined in
two files is a build error. There are no cross-file `$ref`s: always `$ref: '#/components/schemas/Name'`.

## Paths

- Keys are relative to the server URL `/api/v1`, with `:param` written as `{param}`: the mock's
  `/api/v1/orgs/:orgId/procurement/:id` is `/orgs/{orgId}/procurement/{id}`. The mock prefixes some files with
  `/api/v1/orgs/:orgId` inside their local `api()` helper; use the full resulting path.
- Every operation has: `operationId` (camelCase, unique, `<verb><Resource>`, e.g. `listRfqs`, `acceptQuote`),
  `summary` (English, imperative, short), `description` when behaviour is not obvious (business rules, side effects,
  notifications created), one of `tags` from base.yaml, `x-access` (see base.yaml), and `x-audit: true` when the
  mock writes an audit entry.
- Document **every** response the handler can produce: 2xx body, and each 4xx with its `error.code`. Use the shared
  responses and put the specific codes in the description:
  ```yaml
  '409': { $ref: '#/components/responses/Conflict', description: 'invalid_transition: action not available in the current status.' }
  '422': { $ref: '#/components/responses/Validation', description: 'fields: reason (required, min 10 chars).' }
  ```
  Always include `401` where the handler requires a session and `403` where it checks a role/capability/permission.
- Request bodies: reference a named schema. Inline `{type: object, ...}` is fine only for tiny bodies (1-2 fields).
  Mark `required` properly (what the handler rejects with 422 when missing). Put validation limits in the schema
  (`minimum`, `maxLength`, `enum`) mirroring the handler.
- Paginated lists: use the shared `Page`/`PageSize`/`Q`/`Category`/`Region` parameters and a response of
  `{ data: [Item], meta: PageMeta }` defined as a schema named `<Item>Page` in your own schema file.
- 204 responses have no content. Create returns 201.

## Schemas

- **Schema name == exported TypeScript type name** (`TransactionDetail`, `Rfq`, `OrgAuction`, ...). Other agents refer to
  schemas by that exact name without checking your file, so do not rename, and do not skip a type that is used by
  an endpoint response or request.
- Translate the type faithfully: optional `?` means not in `required`; union of string literals is an `enum`;
  discriminated unions are `oneOf` with a `discriminator`; `Partial/Pick/Omit/&` are expanded into explicit
  properties (or `allOf`) rather than approximated; `Record<K, V>` is `additionalProperties`; `string` ISO dates get
  `format: date-time`; `Idr` money fields are `type: integer`. Keep the TypeScript doc comments as `description`.
- Status enums: `src/domain/status.ts` holds every status union (`TransactionStatus`, `AuctionStatus`, ...). The enum
  schemas for those are owned by the types-schema agent; refer to them by name.
- A request/response shape that exists only inline in a handler or hook (not an exported type) is defined by the **path
  agent** in `openapi/schemas/paths-<area>.yaml` with the area prefix (`PersonalFinanceWithdrawal`), never colliding
  with the TS-type names above.

## Do not

- Do not invent behaviour the mock does not have. If something looks wrong or undefined in the mock, keep the mock's
  behaviour and add `x-note:` on the operation.
- Do not touch the frontend repo, `base.yaml`, or other agents' files.
- Do not write Go.
