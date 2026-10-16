// Command idxa is a read-only Postgres index advisor.
package main

import (
	"os"

	"github.com/indexbeacon/indexbeacon/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
