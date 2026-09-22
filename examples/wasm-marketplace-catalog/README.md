# Example WASM marketplace catalog

Closes the empty-NomiHub gap without standing up `hub.nomi.ai`: build a
signed `index.json` + `echo.nomi-plugin` from the in-tree `echo.wasm`
fixture, serve it locally, and point nomid at it.

## Generate

```bash
go run ./examples/wasm-marketplace-catalog/gen
```

Writes `examples/wasm-marketplace-catalog/dist/`:

| File | Purpose |
|---|---|
| `index.json` | Signed catalog (hub.Client wire format) |
| `bundles/echo.nomi-plugin` | Signed WASM bundle |
| `root.pub.b64` | Root pubkey → `NOMI_MARKETPLACE_ROOT_KEY` |
| `root.priv.b64` | DEV ONLY — re-sign; never ship |

Override the bundle base URL if you serve elsewhere:

```bash
NOMI_EXAMPLE_CATALOG_BASE_URL=https://pages.example/bundles \
  go run ./examples/wasm-marketplace-catalog/gen
```

## Serve + wire

```bash
python3 -m http.server -d examples/wasm-marketplace-catalog/dist 8765
export NOMI_MARKETPLACE_ROOT_KEY="$(tr -d '\n' < examples/wasm-marketplace-catalog/dist/root.pub.b64)"
# start nomid with that env
```

In the desktop app: **Settings → Plugins → Browse marketplace → Catalog URL**
→ `http://127.0.0.1:8765/index.json` → Save → Install **Echo (example)**.

Or via API:

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url":"http://127.0.0.1:8765/index.json"}' \
  http://127.0.0.1:8080/settings/marketplace-catalog
```

## Publish your own

Use `nomi-publish catalog` against a directory of signed `.nomi-plugin`
files (same format). The daemon accepts any https (or localhost http)
URL via `marketplace_catalog_url` / the Settings field above.
