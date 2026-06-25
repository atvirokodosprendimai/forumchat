// Module forumchat/sdk-go is a tiny, dependency-free Go client for forumchat
// "external connector" chat bots. It is intentionally standalone (stdlib only)
// so an external worker can `go get` it without pulling the server in — the
// server's own connector code lives under internal/ and is, by Go's rules,
// unimportable from outside the repo. This SDK therefore re-implements the two
// small HMAC primitives the wire needs (see sign.go) rather than sharing them.
module github.com/atvirokodosprendimai/forumchat/sdk-go

go 1.23
