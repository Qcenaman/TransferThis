module transferthis

go 1.24.7

replace golang.org/x/sys => github.com/golang/sys v0.37.0

require (
	github.com/tailscale/walk v0.0.0-20260702185836-28b80ea70d3b
	golang.org/x/sys v0.37.0
)

require (
	github.com/dblohm7/wingoes v0.0.0-20231019175336-f6e33aa7cc34 // indirect
	github.com/tailscale/win v0.0.0-20260619195133-2d76c33a64c1 // indirect
	golang.org/x/exp v0.0.0-20230425010034-47ecfdc1ba53 // indirect
	gopkg.in/Knetic/govaluate.v3 v3.0.0 // indirect
)

replace golang.org/x/exp => github.com/golang/exp v0.0.0-20230425010034-47ecfdc1ba53

replace gopkg.in/Knetic/govaluate.v3 => github.com/Knetic/govaluate v3.0.0+incompatible
