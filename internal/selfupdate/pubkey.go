package selfupdate

// PublicKey is the release signing key's public half (minisign format, the one-line base64 form): every release's
// checksums.txt is signed with its secret half, which lives only in the `release` environment's secret
// TEAMFLOW_SIGNING_KEY (docs/VERIFY.md) and the maintainer's offline backup.
// install.sh carries the same key in its PUBKEY line; a test keeps the two equal.
const PublicKey = "RWRQtNXqbjLlVR4Zk925MGQ1hGBxx+7TI3ebJgFrY7oPQSLRwnbswr57"
