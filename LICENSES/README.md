# Third-Party Licensing

This directory contains the third-party license files for Go module dependencies
reachable from the repository's tracked `go.mod` files.

Only legal files shipped in the downloaded module source trees are collected
here, with a few narrow built-in exceptions in the generator for modules that
need special handling.

To refresh this tree, run `make -C go-controller third-party-licenses`.
To verify that the checked-in tree is up to date, run
`make -C go-controller verify-third-party-licenses`.
