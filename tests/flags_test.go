package tests

import "flag"

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite the .expected golden files")
