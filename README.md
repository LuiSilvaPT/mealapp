# mealapp — budget-aware meal builder

A tiny Go web app that builds meals from the live Glovo/Auchan (Castelo Branco)
catalogue. You set a budget, define your own slots (breakfast, lunch, dinner,
snack, whatever), and it only ever shows products that fit the money you have
left.

Single binary, standard library only, no framework, no build step for the UI.

## Run locally

```
go run .                 # http://localhost:8080
go run . -port 9000
go run . -refresh        # re-crawl Glovo on startup, then serve
```

Two catalogues ship with the app:

- `glovo_catalog.json` — ~6,400 products from the Auchan Castelo Branco store on Glovo, served at `/`.
- `auchan_catalog.json` — ~8,760 products from Auchan's own food aisle (`auchan.pt/pt/alimentacao/`), served at `/auchan-direct`.

Both run the same interface; the URL path picks the data source. Refresh Auchan's with
`./mealapp -crawl-auchan`.

## How it works

| Endpoint | Purpose |
|---|---|
| `GET /` | the app (single embedded page) |
| `GET /api/products?q=&max=&limit=` | matching products, cheapest first, never above `max` |
| `GET /api/stats` | catalogue size, last update, crawl progress |
| `POST /api/refresh` | re-crawl Glovo in the background and rewrite `glovo_catalog.json` |

The crawler walks Glovo's own content API — no account, no cookies, no API key.
It starts at the store front page, follows each food category's `slug`, then
follows collection tiles and lazy content placeholders, collecting every product
with its price, section, unit and photo. A full crawl is roughly 300 requests
and takes 2–7 minutes; it retries on 429/503 with backoff.

## Deploy

The repo builds a static Linux binary in a small Alpine image (`Dockerfile`), so
any container host works.

**Render (free)** — `render.yaml` is a blueprint:
1. Render → New → Blueprint → pick this repo.
2. It reads `render.yaml`, builds the Dockerfile and serves it.
   Free plan sleeps after ~15 min idle, so the first hit takes ~30 s.

**Cloud Run / Fly / any container host:**
```
docker build -t mealapp .
docker run -p 8080:8080 mealapp
```

**Hugging Face Spaces** — create a Docker Space and copy this repo in.

CI runs `go vet`, a build and a smoke test on every push
(`.github/workflows/ci.yml`).

## Notes

- Prices are read live and change; hit **Refresh prices** in the UI before
  ordering.
- Fresh produce and meat are sold by weight, so the final bill can differ.
- The app stores your slots in the browser's localStorage — nothing is sent
  anywhere except the price queries.

