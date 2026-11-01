# drop

Paste links, magnets or text with links in it, or drop `.torrent`, `.nzb`
and `.dlc` files, and drop hands each one to the right download client:

| Input | Client |
|---|---|
| magnet links, `.torrent` URLs and files | Deluge |
| `.nzb` URLs and files | SABnzbd |
| every other link (hosters, video sites, …), `.dlc` containers | JDownloader 2 |

The target can also be picked by hand, with a Deluge label or SABnzbd
category. Below the input, drop shows what the three clients are
downloading: progress, size, speed and time left, refreshed every two
seconds, plus what finished recently.

It's a PWA (installable, and a share target on platforms that support one:
`/share?url=…&text=…`). drop has no login of its own; run it behind a
forward-auth login. Its `POST /api/drop` only accepts requests with an
`X-Drop` header, so other sites can't use a logged-in browser to add
downloads.

## Configuration

Environment variables; a client is used if its URL is set.

| Variable | |
|---|---|
| `JDOWNLOADER_URL` | JDownloader's local API, e.g. `http://jdownloader:3128` (see below) |
| `DELUGE_URL`, `DELUGE_PASSWORD` | Deluge web UI and its password, e.g. `http://deluge:8112` |
| `SABNZBD_URL`, `SABNZBD_API_KEY` | SABnzbd and its API key, e.g. `http://sabnzbd:8080` |
| `PORT` | default `8080` |
| `STATIC_DIR` | the built frontend, default `./static` (`/app/static` in the image) |

### JDownloader without MyJDownloader

drop uses JDownloader 2's local HTTP API, which needs no MyJDownloader
account or connection. In JDownloader: Settings → Advanced Settings, set
`RemoteAPI: Deprecated Api Enabled` to true and `RemoteAPI: Deprecated Api
Localhost Only` to false (port: `RemoteAPI: Deprecated Api Port`, default
3128), then restart JDownloader. The API has no authentication: only let
drop reach that port.

Links are added with autostart and go to JDownloader's default download
folder.

## Development

```sh
cd backend && go test ./... && go run .            # :8080, STATIC_DIR=../frontend/dist
cd frontend && npm install && npm run dev          # Vite, proxies /api to :8080
```

Images (`ghcr.io/dseif0x/drop`, amd64 + arm64) are built on every push to
`main` (tags `main`, `sha-…`, `latest`) and on `v*` tags.
