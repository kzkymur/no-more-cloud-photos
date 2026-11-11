//go:build !nmcp_failpoints

package failpoint

// Hit is an empty function in builds without the nmcp_failpoints tag. Every
// call site inlines to nothing and the linker drops the package.
func Hit(Point) {}
