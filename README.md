# 🎯 AutoApply — AI-powered job search & application assistant

AutoApply searches public job boards, uses an AI model of your choice to **tailor
a résumé and cover letter to every posting**, scores how well you fit, and
prepares each application for your review (or, if you turn it on, applies
automatically). It runs as a **native desktop app** (a single self-contained
`.exe`).

- **Native desktop window** — opens in its own window via the Windows WebView2
  runtime (built into Windows 10/11). No browser tab, no Node, **no C compiler**
  required (it uses a pure-Go WebView2 binding). A browser-based build is also
  available for other platforms.
- **Any job field** — pick from 16 categories spanning **software, healthcare
  (nursing), science & engineering (mechanical, chemist), accounting & finance,
  education, legal, sales, and more**. Not limited to tech.
- **Bring your own AI** — Anthropic (Claude), Google (Gemini), DeepSeek, or any
  **local / OpenAI-compatible** model (Ollama, LM Studio, vLLM, …). Switch
  providers with a dropdown, and **pick the model from a live list** fetched from
  the provider's own API.
- **Keyless job search** — ships with several free sources that need no API key:
  The Muse and Remotive (public APIs) plus **scraped mainstream boards**
  (LinkedIn, Indeed, and best-effort ZipRecruiter / Monster / SimplyHired /
  Craigslist) — not limited to tech. Optional free keys (Adzuna, USAJOBS) add
  precise US ZIP + radius search.
- **Applies at the source** — for each listing the AI resolves the company's
  *own* application URL (Greenhouse / Lever / Ashby / Workday or a careers page)
  and records it, so you apply on the employer's site rather than the board the
  posting was found on.
- **Three-stage AI pipeline** — **Search → Filter → Tailor & apply**. The Filter
  stage is a cheap AI relevance pass that drops obvious mismatches before the
  expensive tailoring. Run each stage from the dashboard, or let automation do
  all three.
- **Connect accounts for better scraping** — optionally sign in to a board in a
  one-click browser window; the app captures that session (cookies + User-Agent,
  **no password stored**) and replays it to get logged-in results and pass
  Cloudflare checks on the harder boards.
- **Browse like a human — no rate-limits or blocks** *(Advanced)* — turn on
  **AI Browser Search (vision)** and the app drives a *real browser* to each
  board's normal search page and has your AI model **read the listings straight
  off screenshots**. Because it browses like a person instead of hitting scrape
  endpoints, it isn't rate-limited or anti-bot-blocked. Needs a **vision-capable**
  model (Claude, Gemini, a GPT-4o-class model, or a local vision model like
  llava); it also replays your connected LinkedIn/ZipRecruiter session.
- **Simple or advanced** — starts in a guided **Simple** mode: upload your résumé,
  set a location, and the AI reads the résumé to pick what to search and runs the
  whole pipeline. Flip to **Advanced** (bottom-left) for full control of sources,
  thresholds, providers and automation.
- **Searches multiple roles at once** — list several targets (e.g.
  `manufacturing / mechanical engineering`) and each is searched separately.
  Duplicates across boards and repeat searches are filtered out automatically.
- **Tailored, truthful materials** — the AI reshapes *your* base résumé to each
  role; it is explicitly instructed never to invent experience you don't have.
- **Review-first by design** — nothing is sent anywhere unless you choose an
  apply channel and opt in. The safe default just prepares materials.

> ⚠️ **Use responsibly.** Auto-submitting applications can violate a job board's
> terms of service and annoy real recruiters. The default *Review* channel
> sends nothing. Keep **Auto-apply** off until you've reviewed several generated
> applications and trust the output. You are responsible for everything sent
> under your name. The scraped boards are intended for personal, non-commercial
> job hunting; keep *Max results per source* modest so you don't hammer them.

---

## Quick start

You need [Go](https://go.dev/dl/) **1.26+** installed. On Windows you also need
the **Microsoft Edge WebView2 runtime** — it's preinstalled on Windows 10/11
(and by any recent Edge), so usually there's nothing to do. If the window fails
to open, install it from Microsoft's "WebView2 Runtime" page.

```powershell
# from the project root (C:\Auto-apply)
go build -o autoapply.exe .
./autoapply.exe          # opens the native desktop window
```

…or run without building a file:

```powershell
go run .
```

**Browser build (no WebView2 / non-Windows):** build with the `headless` tag and
it serves the same GUI to your default browser instead of a native window:

```powershell
go build -tags headless -o autoapply.exe .
./autoapply.exe
```

Once the window (or browser) is up, it opens in **Simple mode**:

- On the **Home** tab, **upload your résumé** (PDF / Word / text) and enter a
  **location**, then click **✨ Find jobs for me**. The AI reads your résumé,
  picks the roles to search, and runs the whole pipeline — jobs stream into the
  **Jobs** tab. Add an AI key under **Settings** first so it can parse and tailor.

Flip to **Advanced** (bottom-left) for the full workflow:

1. **Profile** tab — paste your master résumé into *Base résumé* and fill in your
   contact details and skills. This is the single biggest lever on output quality.
2. **AI & Apply** tab — pick an active provider and enter its API key (or point
   *Local model* at your Ollama/LM Studio server). Click **Test connection**, and
   **↻ Fetch models** to populate the model picker. Optionally **Connect** a
   job-board account here for logged-in / unblocked scraping (see below).
3. **Job Focus** tab — pick your **job categories** (and optional keywords),
   locations, and which sources to use.
4. **Dashboard** — work the three-stage pipeline: **Search now** → **Filter with
   AI** (drops obvious mismatches) → **Tailor matched** (writes the résumé + cover
   letter and resolves the official apply page). Or hit **⚡ Run full pipeline** to
   do all three. Open any job to review and apply.

Settings and discovered jobs are saved to your per-user config directory
(`%AppData%\autoapply\` on Windows). Pass `-data <dir>` to override.

### Command-line flags

| Flag      | Default            | Description                                  |
|-----------|--------------------|----------------------------------------------|
| `-addr`   | `127.0.0.1:8765`   | Address to listen on (falls back to a free port if taken). |
| `-open`   | `true`             | (browser build only) Open the GUI in your browser on start. |
| `-data`   | per-user config dir| Where `config.json` / `data.json` are stored.|

---

## AI providers

Configure these in the **AI & Apply** tab. Models are editable — update them to
whatever your account/host supports.

| Provider   | What you need                          | Default model        |
|------------|----------------------------------------|----------------------|
| Anthropic  | API key (`sk-ant-…`)                   | `claude-sonnet-4-6`  |
| Google     | Gemini API key (`AIza…`)               | `gemini-2.0-flash`   |
| DeepSeek   | API key (`sk-…`)                       | `deepseek-v4-flash`  |
| Local      | Base URL of an OpenAI-compatible server| `llama3.1`           |

**Local models (free, private, offline):**
- **Ollama** — run `ollama serve`, then set Base URL `http://localhost:11434/v1`
  and Model to a pulled model (e.g. `llama3.1`). No API key needed.
- **LM Studio** — start its local server and set Base URL `http://localhost:1234/v1`.

---

## Job categories & multi-field search

In the **Job Focus** tab you select one or more **categories**, which cover any
industry — not just tech:

> Software Engineering · Data Science · Science and Engineering · Healthcare ·
> Accounting and Finance · Sales · Marketing · Design and UX · Human Resources
> and Recruitment · Project Management · Education · Legal · Customer Service ·
> Business Operations · Product Management · Administration and Office

- **The Muse** source filters on these categories server-side, so picking
  *Healthcare* returns nurses, pharmacists and clinicians; *Science and
  Engineering* returns mechanical/electrical engineers and chemists;
  *Accounting and Finance* returns accountants and auditors, and so on.
- The scraped boards (LinkedIn, Indeed, ZipRecruiter, Monster, SimplyHired,
  Craigslist) are keyword search engines, so they query your free-text interest
  directly (plus your location/radius). Remotive maps it onto its own taxonomy.
- **Keywords** are an *optional* refinement: a job is kept if it matches **any
  keyword OR any category**. Leave keywords blank to search by category alone, or
  add e.g. `ICU`, `tax`, `CAD` to narrow within a category.

Job sources:

| Source       | Coverage                                          | Key?     |
|--------------|---------------------------------------------------|----------|
| The Muse     | All fields, category filter (public API)          | no       |
| LinkedIn     | All fields, US + remote (scraped guest endpoint)  | no       |
| Indeed       | All fields, US + remote (scraped mobile API)      | no       |
| ZipRecruiter | All fields (scraped; best-effort)                 | no       |
| SimplyHired  | All fields (scraped; best-effort)                 | no       |
| Monster      | All fields (JS-rendered in a headless browser)    | no       |
| Craigslist   | Local / non-software roles by metro (scraped RSS) | no       |
| AI Browser (vision) | Real browser + vision AI reads any board's results page | no (needs vision AI) |
| Remotive     | US-eligible remote (public API)                   | no       |
| Adzuna       | US, ZIP + radius + salary                         | free key |
| USAJOBS      | US federal                                        | free key |

**Scraped boards are best-effort.** LinkedIn and Indeed are the most reliable
keyless sources. The others (ZipRecruiter, SimplyHired, Monster, Craigslist) sit
behind Cloudflare / anti-bot and may return nothing from some networks (data
centers, VPNs); when that happens the search just logs a note and continues with
the rest. They're left off by default — enable them in **Job Focus** and see
what works from your connection.

### Connected accounts (enhanced / unblocked scraping)

If a board is blocked or you want logged-in results, open **AI & Apply →
Connected accounts** and click **Connect** on a source. A fresh browser window
opens at that board; sign in (you handle 2FA/captchas) or just clear any "are you
human?" check, and the app captures that browser session — its cookies **and**
User-Agent — automatically. **No password is ever stored or seen by the app.**

The captured session is replayed on later scrapes, which:
- **signs you in** (e.g. LinkedIn), and
- **passes Cloudflare** on the harder boards — the captured `cf_clearance` cookie
  is valid because it's replayed with the exact User-Agent and (your) IP it was
  issued to. This is why ZipRecruiter/SimplyHired/Monster/Craigslist often start
  working once connected.

Requires Microsoft Edge or Google Chrome installed (the app drives it via the
DevTools protocol using a pure-Go binding — no extra runtime). Sessions expire;
just **Reconnect** when a source starts failing again.

### AI Browser Search (vision) — the un-blockable source

The scraped boards above talk to web/mobile endpoints over plain HTTP, which is
what anti-bot systems rate-limit and block. **AI Browser Search** sidesteps that
entirely: it drives a **real browser** (Edge/Chrome, via the same pure-Go DevTools
binding) to a board's *ordinary* search-results page — exactly the URL a person
would open — lets the page render, **screenshots it**, and sends the screenshots
to your active AI model's **vision** API, which reads the listings off the images
and returns them as structured jobs. The page's links are passed along too, so the
model can attach the real posting URL to each listing it sees.

Why this is robust:

- **No scrape endpoint, no signature to block.** It requests the same HTML/JS a
  browser does and renders it fully, so there's nothing for rate-limiters to flag.
- **CSS changes don't break it.** Vision reads the rendered card, not brittle
  selectors — when a board reshuffles its markup, the scraper breaks but vision
  keeps working.
- **Reuses your session.** Any connected LinkedIn/ZipRecruiter cookies + UA are
  replayed in the browser, so you get logged-in results and pass Cloudflare.

Turn it on in **Job Focus** (Advanced): enable **AI Browser Search (vision)** in
the source list, then open **🤖 AI Browser Search (vision) — options** to choose
which boards to browse (Indeed, LinkedIn, ZipRecruiter, Google Jobs), how many
screens to capture per board, and whether to show the browser window. It needs a
**vision-capable** provider — Anthropic Claude, Google Gemini, an OpenAI-compatible
GPT-4o-class model, or a local vision model (e.g. llava in Ollama). It's slower and
uses more tokens than the API sources (it sends images), so it's **off by default**
and caps roles/screens to keep each run sane.

---

## How applying works

When an application reaches **Ready**, the **apply channel** (AI & Apply tab)
decides what happens when you apply:

- **Review** *(default)* — prepares materials and marks the job applied when you
  click; you submit on the job site yourself. Nothing is sent automatically.
- **Export** — writes `resume.md`, `cover_letter.txt`, and `job.txt` to a folder
  per job (default: `<data dir>\applications\`).
- **Email** — emails the cover letter with the résumé attached to the posting's
  apply address, using your SMTP settings. Only works when the posting exposes an
  email and SMTP is configured.

**Apply at the company's own site.** Whatever the channel, AutoApply resolves and
records each posting's **official application URL** — the link on the company's
own website or applicant-tracking system (Greenhouse, Lever, Ashby, Workday,
SmartRecruiters, …) rather than the board it was discovered on. It does this from
links in the listing, the employer site (Indeed exposes it), and — when an AI
provider is set — by asking the model. In a job's detail view, **🏢 Open official
application** opens it and **🔎 Find official apply page** resolves it on demand
(also crawling the company's careers page). The URL is recorded only; nothing is
auto-submitted.

**Auto-mode** runs the whole pipeline (search → tailor → optionally apply) on a
timer. **Auto-apply** must also be enabled for it to act via the chosen channel;
otherwise it just keeps your review queue full.

---

## Project layout

```
main.go                 entrypoint: wiring, embedded GUI, server startup
ui_native_windows.go    native WebView2 desktop window (default build, Windows)
ui_browser.go           browser fallback (non-Windows, or `-tags headless`)
internal/
  config/   Config: candidate profile, job focus, AI + apply settings (JSON store)
  store/    Persistent jobs + applications (JSON store)
  ai/       Provider interface — text Generate + Vision (image input) + live
            model listing — over Anthropic / Google / OpenAI-compatible backends
  browser/  Edge/Chrome via chromedp: session capture (connect accounts),
            headless JS rendering (Monster), and search.go (vision-search
            Session: screenshot a page + harvest its links)
  jobs/     Source interface + aggregator + sources (The Muse, LinkedIn, Indeed,
            ZipRecruiter, Monster, SimplyHired, Craigslist, AI Browser/vision,
            Remotive, Adzuna, USAJOBS); scrape.go (shared scraping + JSON-LD),
            visionbrowser.go (screenshot → vision-AI extraction), company.go
            (official-apply-URL resolver), accounts.go (connect-account specs +
            cookie replay), category definitions/synonyms
  resume/   AI prompts: tailoring, prescreen filter, apply-URL pick, résumé parse
  engine/   Orchestration (search → filter → tailor → apply), résumé import,
            automation loop, event hub
  server/   HTTP API + Server-Sent Events + static file serving
web/        Embedded single-page GUI (vanilla HTML/CSS/JS, no build step)
```

### Adding a new job source

Implement the `jobs.Source` interface (`ID`, `Name`, `NeedsCredentials`,
`Search`) in a new file under `internal/jobs/`, then add it to `jobs.Registry`
and `jobs.sourceOrder`. It will appear as a checkbox in the Job Focus tab
automatically. Examples: `themuse.go` (category-aware API), `remotive.go`
(keyword API), `linkedin.go` / `indeed.go` (scraped boards). For scraping, reuse
the helpers in `scrape.go` — `getDoc`/`fetch` (browser-like headers) and
`extractJSONLDJobs` (parses schema.org `JobPosting` markup, which many boards and
company career pages embed). Return a friendly `error` if a board blocks you; the
aggregator logs it as a note and keeps the other sources running.

### Adding a new AI provider

Implement the `ai.Provider` interface (`Name`, `Generate`) and wire it into
`ai.New` plus a `ProviderConfig` in `internal/config`. OpenAI-compatible
services usually need no new code — just set the Local provider's Base URL.

---

## Notes & limitations

- Job descriptions from the boards are HTML; they're converted to plain text
  before being sent to the model.
- AI generation requires a configured provider; search, browsing, and export of
  already-generated materials work without one.
- This is a single-user desktop tool. The API has no auth because it binds to
  localhost only — don't expose it to a public interface.
- The native window needs the WebView2 runtime (standard on Windows 10/11). If
  it's missing, the app keeps running and prints a localhost URL you can open in
  a browser; or rebuild with `-tags headless`.
