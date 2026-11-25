package selfupdate

// PublicKey is the release signing key's public half (minisign format, the one-line base64 form): every release's
// checksums.txt is signed with its secret half, which lives only in the teamflow-cli repo's Actions secret
// TEAMFLOW_SIGNING_KEY.
//
// PLACEHOLDER: empty until the release key is made (docs/VERIFY.md, "Making the release key"). While it is empty,
// `teamflow self-update` refuses ("this build has no release key") and the release workflow refuses to sign.
// install.sh carries the same key in its PUBKEY line; a test keeps the two equal.
const PublicKey = ""
