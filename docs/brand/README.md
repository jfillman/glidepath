# Glidepath brand

| File | Use |
|---|---|
| `glidepath-tile.svg` | the small mark (gauge, glide slope, touchdown) used on the site and in Tower |
| `glidepath-social-preview.png` | the repository's social preview |
| `glidepath-logo.svg` | the GitHub App logo's source |
| `glidepath-logo-512.png` | the Pipelines-as-Code GitHub App's logo (`cicd-pac`; Settings > Display information; GitHub wants a square image under 1 MB) |

The App logo is an amber `G` drawn as a gauge arc open at the top right, its bar the runway, and a sky-blue glide path
coming in through the opening to touch down on the bar: a change landing through the pipeline. It shares Clearance's
App logo style and Hangar's dark theme: ground `#14181D`, amber `#E8A33D`, sky `#6FB2D9`. Use `#14181D` as the App's
badge background color.

Re-render after editing the SVG (macOS):

```
qlmanage -t -s 512 -o /tmp docs/brand/glidepath-logo.svg && cp /tmp/glidepath-logo.svg.png docs/brand/glidepath-logo-512.png
```
