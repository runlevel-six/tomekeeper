package httpclient

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// Refusing to fetch the machine's own neighborhood.
//
// Everything else in this package is about being welcome on servers this archive
// does not own. This file is about the opposite direction: a request the archive
// makes on somebody else's instruction must not become a way to read things only
// this machine can reach.
//
// The failure is not hypothetical. Found on the live archive on 2026-08-23:
// `thetruthaboutguns.com` redirected a feed poll to `http://127.0.0.1`, and the
// fetcher dialed it — five times, with the backoff working exactly as designed
// around a destination it should never have tried. That one hit a closed port. The
// worker pod can also reach the Kubernetes API on 10.43.0.1 and Postgres beside it,
// and a body fetched from either would have been stored as an article and rendered
// in the reading interface.
//
// **Two layers, because they see different things.**
//
//   - `CheckRedirect` sees the hop and can say what was refused and why, which is
//     what makes a legitimately misconfigured feed diagnosable from the attention
//     queue. It sees a host *name* it has not resolved, so it cannot be the guard.
//   - The dial hook sees every address the resolver returned, whoever asked and
//     however they got there: a redirect, a reader pasting an address into **Save
//     a page**, or a public host name that resolves to 10.43.0.1 — rebinding, which
//     no amount of URL checking can catch because the URL is honest.
//
// So the dial hook is the guard and the redirect check is the explanation. Neither
// is sufficient: without the hook the reader-supplied and rebinding cases stay open;
// without the check every refusal reads as "connection refused" against an address
// nobody typed.
//
// **What this does not cover.** A page fetched through the headless browser is
// fetched by Chrome, in its own Deployment, over CDP — not through this transport.
// Restricting where *that* can reach is a NetworkPolicy on the render Deployment
// rather than anything expressible here, and it is written down in
// `explanation/politeness-and-rate-limiting.md` rather than silently assumed.

// ErrPrivateAddress means the destination is not a public address and no allowance
// covers it.
//
// A distinct error, like ErrDisallowedByRobots, because it is not a transport
// failure and will not come good on retry: the address will still be internal in
// twenty minutes. The article is recorded with a reason a person can act on.
var ErrPrivateAddress = errors.New("refused: not a public address")

// PrivateAllowance is the set of otherwise-refused destinations an operator has
// deliberately opened, parsed from TOME_FETCH_ALLOW_PRIVATE.
//
// The zero value allows nothing, which is the default and the safe direction: an
// archive that has never heard of this setting cannot be talked into fetching from
// its own cluster.
type PrivateAllowance struct {
	// prefixes are networks allowed by address, so they hold however a name
	// resolves.
	prefixes []netip.Prefix

	// hosts are allowed by name, which is a weaker claim and deliberately so: a
	// name allowance is checked before the address is known, so it is trust in
	// whatever that name resolves to. It exists because "archive my own wiki at
	// wiki.lan" is the real request, and its address is a DHCP lease.
	hosts map[string]bool
}

// ParsePrivateAllowance reads the comma-separated form: CIDR blocks, bare
// addresses, and host names, in any mixture.
//
// A bare address is accepted as a single-address prefix rather than as a name,
// because `10.0.0.5` is obviously an address and treating it as a host name would
// mean it silently never matched.
func ParsePrivateAllowance(spec string) (PrivateAllowance, error) {
	var a PrivateAllowance

	for _, field := range strings.Split(spec, ",") {
		entry := strings.ToLower(strings.TrimSpace(field))
		if entry == "" {
			continue
		}

		if prefix, err := netip.ParsePrefix(entry); err == nil {
			// Masked, so 10.0.0.5/8 means the network the operator plainly meant
			// rather than never matching anything. netip.Prefix.Contains reports
			// false for a prefix with bits set below its mask.
			a.prefixes = append(a.prefixes, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			a.prefixes = append(a.prefixes, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		if strings.ContainsAny(entry, "/ :") {
			// Nearly a network and not a host name. Worth refusing rather than
			// accepting as a name that cannot match, which is the failure mode that
			// looks like the setting being ignored.
			return PrivateAllowance{}, fmt.Errorf("%q is neither an address, a network, nor a host name", field)
		}
		if a.hosts == nil {
			a.hosts = make(map[string]bool)
		}
		a.hosts[entry] = true
	}

	return a, nil
}

// LoopbackAllowance permits 127.0.0.0/8 and ::1 and nothing else.
//
// Exported for tests, which are the honest instance of the case this whole setting
// exists for: a test server on loopback is a destination somebody deliberately
// pointed this client at. Every fetch test therefore says so, rather than the guard
// carrying an exemption that would also be in force in production.
func LoopbackAllowance() PrivateAllowance {
	return PrivateAllowance{prefixes: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}}
}

// Empty reports whether this allowance permits nothing, which is what makes the
// default sayable in a log line at startup.
func (a PrivateAllowance) Empty() bool { return len(a.prefixes) == 0 && len(a.hosts) == 0 }

// String renders the allowance for a startup log, not for round-tripping.
func (a PrivateAllowance) String() string {
	if a.Empty() {
		return "none"
	}
	parts := make([]string, 0, len(a.prefixes)+len(a.hosts))
	for _, p := range a.prefixes {
		parts = append(parts, p.String())
	}
	for h := range a.hosts {
		parts = append(parts, h)
	}
	return strings.Join(parts, ",")
}

// allowsAddr reports whether an allowance covers a resolved address.
func (a PrivateAllowance) allowsAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range a.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// allowsHost reports whether an allowance names this host.
//
// A trailing dot is stripped, because `wiki.lan.` and `wiki.lan` are the same host
// and only one of them is what somebody would have written in the setting.
func (a PrivateAllowance) allowsHost(host string) bool {
	if len(a.hosts) == 0 {
		return false
	}
	return a.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// refusedPrefixes are the ranges no article, feed or image is ever fetched from.
//
// Written as a table with a reason each, rather than as a chain of netip's
// predicates, because the reason is the part worth reviewing: every entry here is
// somewhere a request could reach that is not the public internet, and the argument
// for each is different.
var refusedPrefixes = []struct {
	prefix netip.Prefix
	why    string
}{
	// The machine itself. This is the one the live incident hit.
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("::1/128"), "loopback"},

	// "This host, this network" — dialing it means dialing something local.
	{netip.MustParsePrefix("0.0.0.0/8"), "unspecified"},
	{netip.MustParsePrefix("::/128"), "unspecified"},

	// RFC1918 and IPv6 unique-local: the cluster, the LAN, the NAS.
	{netip.MustParsePrefix("10.0.0.0/8"), "private"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private"},
	{netip.MustParsePrefix("fc00::/7"), "unique local"},

	// Link-local. 169.254.169.254 is the cloud metadata service, which is the
	// single most valuable address an SSRF can reach on a hosted machine.
	{netip.MustParsePrefix("169.254.0.0/16"), "link local"},
	{netip.MustParsePrefix("fe80::/10"), "link local"},

	// Carrier-grade NAT. Not the public internet, and on a home connection behind
	// CGNAT it is other customers of the same ISP.
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},

	// Reserved ranges that resolve to nothing legitimate and exist in resolver
	// answers mostly by accident or by intent.
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("255.255.255.255/32"), "broadcast"},

	// NAT64. An address here is a v4 destination wearing a v6 address, so
	// refusing v4's private ranges without this leaves the same destinations
	// reachable by another spelling.
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64"},
}

// publicAddr reports whether an address may be fetched from, and why not when it
// may not.
func publicAddr(addr netip.Addr) (bool, string) {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false, "not an address"
	}
	// Multicast covers a family of ranges in both protocols, and none of them is a
	// web server. Checked as a predicate because netip already knows them all.
	if addr.IsMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsLinkLocalMulticast() {
		return false, "multicast"
	}
	for _, r := range refusedPrefixes {
		if r.prefix.Contains(addr) {
			return false, r.why
		}
	}
	return true, ""
}

// guardAddress is the dial hook: it runs once per address the resolver returned,
// after resolution and before the connection.
//
// That position is the whole point. A host name is checked at the only moment its
// actual destination is known, so a name that resolves to the cluster is refused
// however plausible the name was — and a name whose second answer is internal is
// refused on that answer rather than on the first.
func guardAddress(allow PrivateAllowance, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot read the destination %q", ErrPrivateAddress, address)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		// The dial hook is called with a literal address; anything else means the
		// resolver was bypassed. Refusing is the safe reading of a case that should
		// not arise.
		return fmt.Errorf("%w: %q is not a resolved address", ErrPrivateAddress, host)
	}

	if allow.allowsAddr(addr) {
		return nil
	}
	if ok, why := publicAddr(addr); !ok {
		return fmt.Errorf("%w: %s is %s", ErrPrivateAddress, addr, why)
	}
	return nil
}

// checkRedirect refuses a hop the guard would refuse anyway, so that the reason
// reaches the reader instead of a dial error against an address they never saw.
//
// It also refuses a scheme this archive does not fetch. Go's transport would refuse
// those too, and the error it gives names the scheme rather than the redirect that
// introduced it.
func checkRedirect(allow PrivateAllowance, target *url.URL) error {
	switch target.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: redirected to a %s:// address", ErrPrivateAddress, target.Scheme)
	}

	host := target.Hostname()
	if allow.allowsHost(host) {
		return nil
	}

	// Only an address literal can be judged here, and the check is written around
	// that rather than around the parse failing: a host name is not an error, it is
	// the ordinary case, and it is left to the dial hook, which will see what the
	// name resolves to.
	if addr, err := netip.ParseAddr(host); err == nil {
		if allow.allowsAddr(addr) {
			return nil
		}
		if ok, why := publicAddr(addr); !ok {
			return fmt.Errorf("%w: redirected to %s, which is %s", ErrPrivateAddress, addr, why)
		}
	}
	return nil
}
