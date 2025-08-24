# Custom Data Receiver

A small, service-first hub for receiving provider webhooks (starting with Rekor/OpenALPR), storing normalized events and images, and exposing them to a local dashboard and downstream apps.

## Quick start (dev)
1. Install Go 1.22+, Git, ngrok.
2. Copy config.example.yaml to config.yaml and edit as needed.
3. Run: go run ./cmd/hub
4. In another terminal: 
grok http 8080
5. Point Rekor webhook to the ngrok HTTPS URL + /webhooks/rekor/<token>.

## Repo conventions
- Trunk-based with short-lived feature branches.
- Conventional Commits.
- Keep a Changelog format in CHANGELOG.md.