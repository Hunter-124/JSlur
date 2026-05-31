# AutoApply

AI-powered US job search assistant. It searches public job boards, scores fit, tailors your résumé and cover letter per posting, resolves each employer’s official apply URL, and prepares applications for review (or optional auto-apply). Ships as a **native Windows desktop app** (single `autoapply.exe` via [Wails](https://wails.io/) + WebView2).

## Features

- **Wails desktop UI** — frameless window with embedded `web/` UI; API calls run in-process (no localhost server). Requires Go **1.26+**, Wails v2 CLI, and WebView2 (preinstalled on Windows 10/11).
- **Simple & Advanced modes** — upload a résumé on **Home** and run the full pipeline in one click, or use **Dashboard** / **Job Focus** / **Profile** / **Settings** for full control.
- **Any field** — describe target roles in free text; the AI maps your interest onto each board’s category taxonomy (with a keyword fallback when no AI key is set). Separate roles with `/`, `;`, `|`, or newlines.
- **US location search** — city/state or ZIP + radius, optional remote US roles, exclude keywords, min salary (Adzuna). Results are de-duplicated and filtered by an offline radius check so distant postings are dropped.
- **Bring your own AI** — Anthropic, Google Gemini, DeepSeek, or any OpenAI-compatible local server (Ollama, LM Studio, …). Live model list from the provider API.
- **Job sources** — keyless: The Muse, LinkedIn, Indeed, Remotive, plus best-effort scrapes (ZipRecruiter, SimplyHired, Monster, Craigslist). Optional free keys: Adzuna, USAJOBS. **AI Browser Search (vision)** drives a real browser and reads listings from screenshots (vision model required; off by default).
- **Connected accounts** — optional one-click browser sign-in for LinkedIn, ZipRecruiter, SimplyHired, Monster, Craigslist; cookies + User-Agent are replayed (no password stored). Helps logged-in results and Cloudflare-heavy boards.
- **Official apply URLs** — resolves Greenhouse / Lever / Ashby / Workday / careers-page links so you apply on the employer site, not the discovery board.
- **Pipeline** — **Search → Filter → Tailor** (cheap AI relevance filter before expensive tailoring). Run stages from the dashboard or automation on a timer.
- **Apply channels** — **Review** (default), **Export** to disk, or **Email** via SMTP. Auto-mode and auto-apply are opt-in.

> Use responsibly. Scraped boards are for personal job hunting; keep per-source result caps modest. Default is review-only — nothing is sent unless you enable auto-apply and trust the output.

## Quick start

```powershell
# Install Wails CLI once:
go install github.com/wailsapp/wails/v2/cmd/wails@latest

# From the project root:
.\build.bat
.\build\bin\autoapply.exe
```

`build.bat` runs `wails build -platform windows/amd64 -trimpath`. For development: `wails dev`.

**First run (Simple):** add an AI key under **Settings**, upload your résumé on **Home**, set location, click **Find jobs for me** (search → filter → tailor).

**Advanced:** **Profile** (base résumé), **Job Focus** (roles, location, sources), **Settings** (AI, keys, connected accounts, automation), **Dashboard** (per-stage controls or full pipeline).

Data is stored under `%AppData%\autoapply\` (`config.json`, `data.json`). Exports default to `<data dir>\applications\`.

## AI providers

| Provider  | Credentials        | Default model       |
|-----------|--------------------|---------------------|
| Anthropic | API key            | `claude-sonnet-4-6` |
| Google    | Gemini API key     | `gemini-2.0-flash`  |
| DeepSeek  | API key            | `deepseek-v4-flash` |
| Local     | Base URL (+ opt. key) | `llama3.1` at `http://localhost:11434/v1` |

Tailoring, filtering, résumé parsing, category mapping, and vision browser search need a configured provider. Search without AI still works via heuristics; export of existing materials does not.

## Job sources

| Source | Notes | API key |
|--------|-------|---------|
| The Muse | Category-aware public API | no |
| LinkedIn, Indeed | Scraped; most reliable keyless | no |
| ZipRecruiter, SimplyHired, Monster, Craigslist | Best-effort; often blocked without connected session | no |
| Remotive | US-eligible remote (public API) | no |
| AI Browser (vision) | Real browser + vision model; Indeed, LinkedIn, ZipRecruiter, Google Jobs | no (needs vision AI) |
| Adzuna | US ZIP + radius + salary | [free key](https://developer.adzuna.com/) |
| USAJOBS | US federal | [free key](https://developer.usajobs.gov/) |

Defaults enable The Muse, LinkedIn, Indeed, and Remotive. Harder scrapes are off until you enable them in **Job Focus**.

## Project layout

```
main.go, app.go          Wails entrypoint; in-process API bridge + events
window_windows.go        Frameless window rounding (Windows)
internal/
  config/   Persisted profile, job focus, AI, sources, apply settings
  store/    Jobs and applications (JSON)
  ai/       Anthropic, Google, OpenAI-compatible backends + vision
  browser/  Chrome/Edge via chromedp: connect accounts, vision search
  jobs/     Sources, aggregator, scraping, geo filter, company URL resolver
  resume/   Tailoring, prescreen, category selection, résumé import prompts
  engine/   Pipeline orchestration and automation
  server/   HTTP handler (routes used by the Wails bridge, not a network server)
web/        Embedded SPA (HTML/CSS/JS)
```

**New job source:** implement `jobs.Source` in `internal/jobs/`, register in `jobs.Registry` and `jobs.sourceOrder`.

**New AI provider:** implement `ai.Provider`, wire in `ai.New` and `internal/config` (OpenAI-compatible hosts usually only need the Local base URL).

## Limitations

- US-focused searches and location filtering.
- Single-user desktop tool; no API authentication (in-process only).
- Windows build is the supported path; other platforms are not packaged here.
- Job descriptions are stripped to plain text before sending to models.
