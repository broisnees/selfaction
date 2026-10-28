// Command hello is the sample the platforms workflow builds and runs on
// Linux, macOS and Windows.
package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Printf("Hello from selfaction on %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
