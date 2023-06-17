# bender

## Features

- Doesn't suck

## Requirements

- nftables
- containerd
- Linux kernel v5.13+ (for nftables cgroupv2 matching)
- `data_dir` must be in a BTRFS filesystem.
  - If you're running as non-root, it must be mounted with the `user_subvol_rm_allowed` option.

## Getting Started

- Depending on where your repos are:
  - If they're in your personal account: go to your personal settings -> Developer settings -> GitHub apps -> New GitHub App
  - If they're go to your organization's Settings -> Developer settings -> GitHub apps -> New GitHub App
- Fill the form like this:
  - GitHub App name: enter some cool name.
  - Homepage URL: the URL where you're going to deploy Bender. e.g. `https://bender.example.com`
  - Webhook URL: The url, with `/webhook` added. e.g. `https://bender.example.com/webhook`
  - Webhook secret: Generate a long and secure random string. For example with `pwgen -s 32`.
  - Repository permissions
    - Commit statuses: Read and write
    - Contents: Read-only
    - Pull requests: Read-only
  - Subscribe to events
    - Pull request
    - Push
  - Where can this GitHub App be installed?: Only on this account.
    - IMPORTANT: If you set it to "Any account" instead, then ANYONE on GitHub will be able to use your CI service on THEIR repos.
- Create
- In "Private keys", click "Generate a private key". Keep the downloaded `.pem` file.
- In the left menu click "Install App"
- Select the repositories you want to use Bender with.

- Write the following into `config.toml`.

```yaml
external_url: https://bender.example.com  # replace
data_dir: data
listen_port: 8000 
image: embassy.dev/ci:latest
net_sandbox:
  allowed_domains:
  - '*.github.com'
  - '*.githubusercontent.com'
github:
  webhook_secret: REPLACE_ME_WITH_YOUR_SECRET  # replace
  app_id: 321321  # replace
  private_key: |  # replace
    -----BEGIN RSA PRIVATE KEY-----
    MIIEpQxxxxxxxxx
    xxxxxxxxxxxxxREPLACE_MExxxxxxxxx
    xxxxxx9N7c=
    -----END RSA PRIVATE KEY-----
```

- Run `bender -c config.toml`