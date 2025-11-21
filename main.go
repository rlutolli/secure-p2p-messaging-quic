// i was here. this will be my legacy, my mark on this world.

package main

import (
	"os"
)

const addr = "localhost:4242" // both address and port used for the server and client

func main() {
	if len(os.Args) > 1 && os.Args[1] == "server" {
		startServer(addr)
	} else {
		runClient(addr)
	}
}
