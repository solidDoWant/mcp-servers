# sendcutsend

Upload flat parts to [SendCutSend](https://sendcutsend.com), price them in any material, and build saved carts that the account owner checks out in a browser.

## Tools

| Tool | What it does |
|---|---|
| `list_materials` | Materials with their SKUs, thicknesses and services. Filters by category, form, service and text. |
| `get_upload_url` | A pre-signed URL to PUT a part file to (DXF, DWG, AI, EPS or STEP), for files too big or too binary to pass inline. |
| `create_part` | Add a part from inline DXF text or an uploaded file, wait for SendCutSend to process it, and report its size and any problems. |
| `get_part`, `list_parts` | The account's parts. |
| `delete_part` | Delete a part. |
| `quote_part` | Price a part for a quantity: in every material, cheapest first, or in one material with price breaks and delivery dates. |
| `create_saved_cart` | Put parts in a saved cart and return its checkout URL. |
| `list_saved_carts`, `get_saved_cart` | The account's saved carts. |
| `delete_saved_cart` | Delete a saved cart. |

Nothing here orders or pays. The user opens the saved cart's checkout URL while signed in, and it loads the parts into their cart to review and pay.

Prices are for cutting only. Services such as bending, tapping, countersinking and finishing aren't supported yet: add them on the website before checking out.

### Saved carts and the account's cart

SendCutSend builds a saved cart from everything in the account's cart; there is no way to choose items. So `create_saved_cart` only runs when that cart is empty. It adds its parts, saves the cart, and takes its own parts out again. It never removes items it didn't add, and it saves nothing if someone adds to the cart meanwhile.

A saved cart can't be changed. To change one, create a new one and delete the old one.

### Uploaded files are public

SendCutSend stores uploads in S3 with a public-read ACL under a random key. Anyone who has a part's file URL can download the file. The site's own upload works the same way.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `MCP_LISTEN_ADDR` | `:8080` | Where to serve MCP (`/mcp`) and the health check (`/healthz`). |
| `SENDCUTSEND_CREDENTIALS_FILE` | none | A file holding the account's `user_credentials` cookie. Required for everything but a test. |
| `SENDCUTSEND_BASE_URL` | `https://app.sendcutsend.com` | The web app. Tests point it at a fake. |

### The credentials file

SendCutSend has no API keys. The server signs in with the `user_credentials` cookie that the site sets on sign-in:

1. Sign in at <https://app.sendcutsend.com>.
2. In the browser's developer tools, copy the value of the `user_credentials` cookie for `.sendcutsend.com`. It looks like `<128 hex digits>%3A%3A<account number>`.
3. Put the value in the credentials file. `user_credentials=<value>` works too.

The cookie stays valid until the account signs out or changes its password, so don't sign out of the browser session you copied it from. When the site stops accepting it, every tool says so; replace the file's content and the next call uses it, with no restart.

Anyone with the cookie can use the account as its owner. Store it as a secret.

### Network

The server needs outbound HTTPS to `app.sendcutsend.com` and `sendcutsend.s3-accelerate.amazonaws.com` (part uploads). Agents that upload with `get_upload_url` need the S3 host too.

## How it works

It calls the JSON API that SendCutSend's web app uses. That API is undocumented and may change.

Each tool call starts a new site session: it loads the app page with the cookie, takes the CSRF token and app settings from it, then makes the API calls. A page that shows a guest session means the cookie was rejected.

The nightly live test (`make test-live`) uploads and prices a part as a guest, which needs no account. Set `SENDCUTSEND_CREDENTIALS_FILE` to also run the account test, which creates a part and a saved cart and deletes both.
