package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/pi"
	"github.com/buildytech/vibechat-desktop/server/internal/contour"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
)

// contourFile is the server's contour config (VIBECHAT_CONTOUR_FILE overrides <data>/contour.json). It holds no secret.
func (p paths) contourFile() string {
	if f := os.Getenv(contour.EnvFile); f != "" {
		return f
	}
	return filepath.Join(p.data, "contour.json")
}

func loadContour(p paths) (contour.Config, error) { return contour.Load(p.contourFile()) }

// egressDomains prints the hostnames the agent traffic of this contour may reach, one per line: the chosen provider's
// and nothing else (the Cursor contour needs none from this tool: its own egress list is the installer's). The server
// owner opens exactly these in the container's network policy.
func egressDomains(p paths, stdout, stderr io.Writer) int {
	c, err := loadContour(p)
	if err != nil {
		fmt.Fprintln(stderr, "egress-domains:", err)
		return 1
	}
	hosts, err := c.EgressHosts()
	if err != nil {
		fmt.Fprintln(stderr, "egress-domains:", err)
		return 1
	}
	if len(hosts) == 0 {
		fmt.Fprintf(stderr, "egress-domains: the %s contour has no provider hosts of its own (%s)\n", c.RuntimeID(), p.contourFile())
		return 0
	}
	for _, h := range hosts {
		fmt.Fprintln(stdout, h)
	}
	return 0
}

// contourChecks are the doctor lines of the contour: the config, and for Pi the pieces that must all be there for the
// agent to run (bubblewrap, Node, the pinned Pi, the provider key) and the hosts to open.
func contourChecks(p paths) []check {
	c, err := loadContour(p)
	if err != nil {
		return []check{bad("contour", err.Error())}
	}
	if !c.UsesPi() {
		if _, serr := os.Stat(p.contourFile()); serr != nil {
			return []check{ok("contour", "cursor-sdk (no "+p.contourFile()+")")}
		}
		return []check{ok("contour", "cursor-sdk ("+p.contourFile()+")")}
	}
	name := c.Contour
	if name == "" {
		name = "custom"
	}
	cs := []check{ok("contour", fmt.Sprintf("%s: runtime pi, provider %s (%s)", name, c.Provider, p.contourFile()))}
	if err := isolate.JailAvailable(); err != nil {
		cs = append(cs, bad("pi jail", "Pi runs only in bubblewrap: "+err.Error()))
	} else {
		cs = append(cs, ok("pi jail", "bubblewrap works"))
	}
	node, dir := c.Pi.Node, c.Pi.Dir
	if node == "" {
		node = os.Getenv(contour.EnvPiNode)
	}
	if dir == "" {
		dir = os.Getenv(contour.EnvPiDir)
	}
	if in, err := pi.Locate(pi.PinnedLock(), node, dir, exec.LookPath); err != nil {
		cs = append(cs, bad("pi", err.Error()))
	} else {
		cs = append(cs, ok("pi", in.Describe()+" at "+in.Dir+", node "+in.Node))
	}
	prov, _, err := c.BuildProvider(contour.Env{})
	if err != nil {
		return append(cs, bad("pi provider", err.Error()))
	}
	if err := prov.Ready(); err != nil {
		cs = append(cs, warn("pi provider", err.Error()+" (the key is read from the service's environment; run doctor with it to check)"))
	} else {
		cs = append(cs, ok("pi provider", c.Provider+": key present in this environment"))
	}
	cs = append(cs, ok("egress", "open only: "+strings.Join(prov.Hosts(), ", ")))
	return cs
}
