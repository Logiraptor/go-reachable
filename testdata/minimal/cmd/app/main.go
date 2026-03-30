package main

import "example.com/minimal/pkg"

func main() {
	pkg.F()
	pkg.G() // keep G in the program so it has a call graph node
}
