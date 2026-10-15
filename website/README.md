# Ai-Shell website

Static product landing page for [Ai-Shell](https://github.com/3thinkthendoit/Ai-Shell).

## Local preview

```bash
# from repo root
npx --yes serve website
# or
python -m http.server 8080 --directory website
```

Open `http://localhost:3000` (or the port printed / `8080`).

## GitHub Pages

In the GitHub repo settings → Pages:

- Source: **Deploy from a branch**
- Branch: `main` (or your default), folder: `/website`

Or push `website/` contents to a `gh-pages` branch.

Product screenshots live in `assets/` (`screenshot-chat.png`, `screenshot-terminal.png`). Keep public IPs and host details masked before adding new shots.
