// tinychat is a runnable example of the forumchat connector SDK: a terminal chat
// client that joins a community as an external connector. It is its own module
// with a local `replace` so it builds straight from the repo; a real external
// app would instead `go get
// github.com/atvirokodosprendimai/forumchat/sdk-go` and drop the replace.
module github.com/atvirokodosprendimai/forumchat/examples/tinychat

go 1.23

require github.com/atvirokodosprendimai/forumchat/sdk-go v0.0.0

replace github.com/atvirokodosprendimai/forumchat/sdk-go => ../../sdk-go
