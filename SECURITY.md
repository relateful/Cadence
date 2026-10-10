# Security policy

## Reporting a vulnerability

Please don't open a public issue for security problems. Report them privately through [GitHub's advisory form](https://github.com/relateful/Cadence/security/advisories/new) instead.

Include what you found, how to reproduce it, and what an attacker could do with it. You'll get a reply within a week, and we'll keep you updated until it's fixed.

## Supported versions

Only the latest commit on `main` receives security fixes.

## Keeping your node safe

- Set a long, random `CADENCE_PASSWORD`. Every request is authorised with it.
- Don't expose Cadence directly to the internet unless you have to. Put it on a private network with your bot, or behind a firewall that only allows your bot's address.
- Never commit your `.env`. It holds your Deezer session and API credentials.
