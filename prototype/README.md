# Relay Deploy - clickable prototype

Five wired pages sharing one app shell. Open `home.html` in a browser
(no server needed) or run `python3 -m http.server 8000` from this folder.

    home.html       Overview - deploy health, build-duration chart, deployments table
    features.html   14 platform capabilities, filterable by category
    pricing.html    Plan tiers, billing-cycle toggle, quota meters, invoices
    docs.html       Rollback article with code tabs and TOC scrollspy
    contact.html    Ticket form with validation, channels, status, open tickets

## What is actually playable

* Sidebar nav links between all five pages; active state follows the page.
* Command palette on every page - `Cmd/Ctrl+K`, or `/`, or click the search bar.
  Arrow keys move, Enter runs, Esc closes. Navigates and fires real actions.
* Features: category tabs filter the capability grid (mouse or arrow keys).
* Pricing: Monthly/Annual toggle rewrites every price, the header total and the
  savings note, and confirms with a toast.
* Docs: CLI/REST/SDK code tabs, copy-to-clipboard, TOC highlights on scroll.
* Contact: ticket form validates on submit, issues a SUP- number, accepts
  dragged-in attachments as removable chips.
* Tables: the filter inputs on Overview and Contact narrow rows live and swap in
  an empty state when nothing matches.
* Every other control answers with a toast describing what it would do.

## Structure

    assets/shell.css    the whole design system - tokens, shell, components
    assets/shell.js     reveal, command palette, toasts, tabs, table filter
    assets/*.js         per-page behaviour (pricing, docs, contact)
    _src/               page sources; `python3 _src/build.py` regenerates the
                        five pages and the self-contained copies in
                        .lucid/design/
    screenshots/        desktop 1440, mobile 390, and four interaction states
    _src/shot.js        re-captures screenshots (needs prototype/.browsers)

Edit `_src/main_*.html` or `assets/shell.css`, then re-run the builder. Never
edit the generated `*.html` at this level directly - the builder overwrites it.
