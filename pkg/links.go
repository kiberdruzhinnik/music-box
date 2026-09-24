package proxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// linkConfig contains the sing-box target produced from one share URL.
type linkConfig struct {
	outbound map[string]any
	endpoint map[string]any
}

// parseShareURL parses a share URL without including it in any error message.
func parseShareURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.ReplaceAll(raw, "&amp;", "&"))
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid share URL or missing server host")
	}
	return parsed, nil
}

// serverPort reads a valid port, using the protocol default when absent.
func serverPort(parsed *url.URL, fallback int) (int, error) {
	if parsed.Port() == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid share URL server port")
	}
	return port, nil
}

// queryFirst returns the first nonempty value among alternative query keys.
func queryFirst(query url.Values, keys ...string) string {
	for _, key := range keys {
		if value := query.Get(key); value != "" {
			return value
		}
	}
	return ""
}

// boolParam accepts the boolean form used by common share URLs.
func boolParam(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// tlsFields converts common TLS, ALPN, uTLS, and Reality parameters.
func tlsFields(query url.Values, host string, reality bool) map[string]any {
	serverName := queryFirst(query, "sni", "peer", "host")
	if serverName == "" {
		serverName = host
	}
	tls := map[string]any{"enabled": true, "server_name": serverName}
	if alpn := query.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if boolParam(queryFirst(query, "allowInsecure", "insecure")) {
		tls["insecure"] = true
	}
	if reality {
		tls["reality"] = map[string]any{
			"enabled": true, "public_key": query.Get("pbk"), "short_id": query.Get("sid"),
		}
	}
	if fingerprint := query.Get("fp"); fingerprint != "" || reality {
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	return tls
}

// transportFields converts V2Ray transport query parameters when present.
func transportFields(query url.Values) map[string]any {
	transportType := query.Get("type")
	host := query.Get("host")
	path := query.Get("path")
	if path == "" {
		path = "/"
	}
	switch transportType {
	case "ws":
		headers := map[string]string{}
		if host != "" {
			headers["Host"] = host
		}
		return map[string]any{"type": "ws", "path": path, "headers": headers}
	case "grpc":
		return map[string]any{"type": "grpc", "service_name": queryFirst(query, "serviceName", "path")}
	case "http":
		hosts := []string{}
		if host != "" {
			hosts = append(hosts, host)
		}
		return map[string]any{"type": "http", "host": hosts, "path": path}
	case "httpupgrade":
		return map[string]any{"type": "httpupgrade", "host": host, "path": path}
	case "quic":
		return map[string]any{"type": "quic"}
	default:
		return nil
	}
}

// jsonString renders VMess JSON values as the strings used in share links.
func jsonString(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// parseVMess builds a VMess outbound from its encoded JSON share link.
func parseVMess(raw string) (map[string]any, error) {
	payload, err := url.PathUnescape(strings.SplitN(strings.TrimPrefix(raw, "vmess://"), "#", 2)[0])
	if err != nil {
		return nil, fmt.Errorf("invalid VMess encoding")
	}
	decoded, err := decodeBase64(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid VMess Base64 payload")
	}
	var data map[string]any
	if json.Unmarshal([]byte(decoded), &data) != nil {
		return nil, fmt.Errorf("invalid VMess JSON payload")
	}
	host := jsonString(data["add"], "")
	port, err := strconv.Atoi(jsonString(data["port"], "443"))
	if err != nil || port < 1 || port > 65535 || host == "" {
		return nil, fmt.Errorf("invalid VMess server or port")
	}
	uuid := jsonString(data["id"], "")
	if uuid == "" {
		return nil, fmt.Errorf("VMess link is missing its user ID")
	}
	alterID, err := strconv.Atoi(jsonString(data["aid"], "0"))
	if err != nil {
		return nil, fmt.Errorf("invalid VMess alter ID")
	}
	outbound := map[string]any{
		"type": "vmess", "tag": "proxy", "server": host, "server_port": port,
		"uuid": uuid, "security": jsonString(data["scy"], "auto"), "alter_id": alterID,
	}
	network := jsonString(data["net"], "tcp")
	hostHeader := jsonString(data["host"], "")
	path := jsonString(data["path"], "/")
	switch network {
	case "ws":
		headers := map[string]string{}
		if hostHeader != "" {
			headers["Host"] = hostHeader
		}
		transport := map[string]any{"type": "ws", "path": path, "headers": headers}
		if earlyData, err := strconv.Atoi(jsonString(data["ed"], "")); err == nil && earlyData > 0 {
			transport["max_early_data"] = earlyData
			transport["early_data_header_name"] = "Sec-WebSocket-Protocol"
		}
		outbound["transport"] = transport
	case "grpc":
		outbound["transport"] = map[string]any{"type": "grpc", "service_name": path}
	case "http":
		hosts := []string{}
		if hostHeader != "" {
			hosts = append(hosts, hostHeader)
		}
		outbound["transport"] = map[string]any{"type": "http", "host": hosts, "path": path}
	case "httpupgrade":
		outbound["transport"] = map[string]any{"type": "httpupgrade", "host": hostHeader, "path": path}
	case "quic":
		outbound["transport"] = map[string]any{"type": "quic"}
	}
	if jsonString(data["tls"], "") == "tls" {
		query := url.Values{}
		for _, key := range []string{"sni", "alpn", "fp"} {
			query.Set(key, jsonString(data[key], ""))
		}
		query.Set("host", hostHeader)
		query.Set("insecure", jsonString(data["skip-cert-verify"], ""))
		outbound["tls"] = tlsFields(query, host, false)
	}
	return outbound, nil
}

// parseShadowsocks reads SIP002 and legacy Shadowsocks share links.
func parseShadowsocks(raw string) (map[string]any, error) {
	parsed, err := url.Parse(strings.ReplaceAll(raw, "&amp;", "&"))
	if err != nil {
		return nil, fmt.Errorf("invalid Shadowsocks share URL")
	}
	var method, password, host string
	var port int
	if parsed.User != nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
		port, err = serverPort(parsed, 443)
		if err != nil {
			return nil, err
		}
		method = parsed.User.Username()
		if plainPassword, ok := parsed.User.Password(); ok {
			password = plainPassword
		} else {
			decoded, decodeErr := decodeBase64(method)
			if decodeErr != nil {
				return nil, fmt.Errorf("invalid Shadowsocks credentials")
			}
			method, password, ok = strings.Cut(decoded, ":")
			if !ok {
				return nil, fmt.Errorf("invalid Shadowsocks credentials")
			}
		}
	} else {
		payload := strings.SplitN(strings.SplitN(strings.TrimPrefix(raw, "ss://"), "#", 2)[0], "?", 2)[0]
		decoded, decodeErr := decodeBase64(payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("invalid legacy Shadowsocks payload")
		}
		credentials, endpoint, ok := strings.Cut(decoded, "@")
		if !ok {
			return nil, fmt.Errorf("invalid legacy Shadowsocks payload")
		}
		method, password, ok = strings.Cut(credentials, ":")
		if !ok {
			return nil, fmt.Errorf("invalid Shadowsocks credentials")
		}
		endpointURL, parseErr := parseShareURL("ss://" + endpoint)
		if parseErr != nil {
			return nil, parseErr
		}
		host = endpointURL.Hostname()
		port, err = serverPort(endpointURL, 443)
		if err != nil {
			return nil, err
		}
	}
	if host == "" || method == "" || password == "" {
		return nil, fmt.Errorf("invalid Shadowsocks server or credentials")
	}
	outbound := map[string]any{
		"type": "shadowsocks", "tag": "proxy", "server": host,
		"server_port": port, "method": method, "password": password,
	}
	query := parsed.Query()
	if pluginValue := query.Get("plugin"); pluginValue != "" {
		plugin, options, hasOptions := strings.Cut(pluginValue, ";")
		outbound["plugin"] = plugin
		if !hasOptions {
			options = queryFirst(query, "plugin_opts", "plugin-opts")
		}
		if options != "" {
			outbound["plugin_opts"] = options
		}
	}
	return outbound, nil
}

// parseProxyLink converts every supported share URL to a sing-box target.
func parseProxyLink(raw string) (linkConfig, error) {
	scheme := strings.ToLower(strings.SplitN(raw, "://", 2)[0])
	if scheme == "vmess" {
		outbound, err := parseVMess(raw)
		return linkConfig{outbound: outbound}, err
	}
	if scheme == "ss" {
		outbound, err := parseShadowsocks(raw)
		return linkConfig{outbound: outbound}, err
	}
	parsed, err := parseShareURL(raw)
	if err != nil {
		return linkConfig{}, err
	}
	query := parsed.Query()
	host := parsed.Hostname()
	defaultPort := 443
	switch scheme {
	case "ssh":
		defaultPort = 22
	case "http":
		defaultPort = 80
	case "socks", "socks4", "socks5":
		defaultPort = 1080
	case "wg":
		defaultPort = 51820
	}
	port, err := serverPort(parsed, defaultPort)
	if err != nil {
		return linkConfig{}, err
	}
	base := map[string]any{"tag": "proxy", "server": host, "server_port": port}
	username := ""
	password := ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	switch scheme {
	case "vless":
		if username == "" {
			return linkConfig{}, fmt.Errorf("VLESS link is missing its user ID")
		}
		base["type"] = "vless"
		base["uuid"] = username
		if flow := query.Get("flow"); flow != "" {
			base["flow"] = flow
		}
		if transport := transportFields(query); transport != nil {
			base["transport"] = transport
		}
		switch query.Get("security") {
		case "tls":
			base["tls"] = tlsFields(query, host, false)
		case "reality":
			base["tls"] = tlsFields(query, host, true)
		}
	case "trojan":
		if username == "" {
			return linkConfig{}, fmt.Errorf("Trojan link is missing its password")
		}
		base["type"] = "trojan"
		base["password"] = username
		base["tls"] = tlsFields(query, host, false)
		if transport := transportFields(query); transport != nil {
			base["transport"] = transport
		}
	case "hy2", "hysteria2":
		if username == "" {
			return linkConfig{}, fmt.Errorf("Hysteria2 link is missing its password")
		}
		base["type"] = "hysteria2"
		base["password"] = username
		base["tls"] = tlsFields(query, host, false)
		if obfs := queryFirst(query, "obfs-password", "obfs"); obfs != "" {
			base["obfs"] = map[string]any{"type": "salamander", "password": obfs}
		}
	case "hysteria":
		base["type"] = "hysteria"
		base["auth_str"] = query.Get("auth")
		base["tls"] = tlsFields(query, host, false)
		if value := query.Get("upmbps"); value != "" {
			up, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				return linkConfig{}, fmt.Errorf("invalid Hysteria upload rate")
			}
			base["up_mbps"] = up
		}
		if value := query.Get("downmbps"); value != "" {
			down, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				return linkConfig{}, fmt.Errorf("invalid Hysteria download rate")
			}
			base["down_mbps"] = down
		}
		if obfs := queryFirst(query, "obfsParam", "obfs"); obfs != "" {
			base["obfs"] = obfs
		}
	case "tuic":
		if username == "" || password == "" {
			return linkConfig{}, fmt.Errorf("TUIC link requires a user ID and password")
		}
		base["type"] = "tuic"
		base["uuid"] = username
		base["password"] = password
		base["tls"] = tlsFields(query, host, false)
		if congestion := query.Get("congestion_control"); congestion != "" {
			base["congestion_control"] = congestion
		}
		if relay := query.Get("udp_relay_mode"); relay != "" {
			base["udp_relay_mode"] = relay
		}
	case "wg":
		if username == "" || query.Get("public_key") == "" {
			return linkConfig{}, fmt.Errorf("WireGuard link requires private and peer public keys")
		}
		peer := map[string]any{
			"address": host, "port": port, "public_key": query.Get("public_key"),
			"allowed_ips": []string{"0.0.0.0/0", "::/0"},
		}
		if reserved := query.Get("reserved"); reserved != "" {
			parts := strings.Split(reserved, ",")
			values := make([]int, 0, len(parts))
			for _, part := range parts {
				value, parseErr := strconv.Atoi(strings.TrimSpace(part))
				if parseErr != nil || value < 0 || value > 255 {
					return linkConfig{}, fmt.Errorf("invalid WireGuard reserved byte")
				}
				values = append(values, value)
			}
			peer["reserved"] = values
		}
		if preSharedKey := query.Get("pre_shared_key"); preSharedKey != "" {
			peer["pre_shared_key"] = preSharedKey
		}
		address := query.Get("local_address")
		if address == "" {
			address = "172.16.0.2/32"
		}
		endpoint := map[string]any{
			"type": "wireguard", "tag": "proxy", "private_key": username,
			"address": strings.Split(address, ","), "peers": []any{peer},
		}
		if mtu := query.Get("mtu"); mtu != "" {
			value, parseErr := strconv.Atoi(mtu)
			if parseErr != nil || value < 1 {
				return linkConfig{}, fmt.Errorf("invalid WireGuard MTU")
			}
			endpoint["mtu"] = value
		}
		return linkConfig{endpoint: endpoint}, nil
	case "ssh":
		if username == "" {
			return linkConfig{}, fmt.Errorf("SSH link is missing its user name")
		}
		base["type"] = "ssh"
		base["user"] = username
		if password != "" {
			base["password"] = password
		}
	case "http", "https":
		base["type"] = "http"
		if username != "" {
			base["username"] = username
		}
		if password != "" {
			base["password"] = password
		}
		if scheme == "https" {
			base["tls"] = map[string]any{"enabled": true, "server_name": host}
		}
	case "socks", "socks4", "socks5":
		base["type"] = "socks"
		version := "5"
		if scheme == "socks4" {
			version = "4"
		}
		base["version"] = version
		if username != "" {
			base["username"] = username
		}
		if password != "" {
			base["password"] = password
		}
	case "naive+https":
		base["type"] = "naive"
		base["tls"] = map[string]any{"enabled": true, "server_name": host}
		if username != "" {
			base["username"] = username
		}
		if password != "" {
			base["password"] = password
		}
	default:
		return linkConfig{}, fmt.Errorf("unsupported share URL scheme")
	}
	return linkConfig{outbound: base}, nil
}

// BuildSingBoxConfig wraps a parsed share URL in loopback HTTP and SOCKS listeners.
func BuildSingBoxConfig(raw string, httpPort, socksPort int) ([]byte, error) {
	target, err := parseProxyLink(raw)
	if err != nil {
		return nil, err
	}
	config := map[string]any{
		"log": map[string]any{"level": "error"},
		"inbounds": []any{
			map[string]any{"type": "http", "tag": "http-in", "listen": "127.0.0.1", "listen_port": httpPort},
			map[string]any{"type": "socks", "tag": "socks-in", "listen": "127.0.0.1", "listen_port": socksPort},
		},
		"route": map[string]any{"final": "proxy"},
	}
	if target.endpoint != nil {
		config["outbounds"] = []any{map[string]any{"type": "direct", "tag": "direct"}}
		config["endpoints"] = []any{target.endpoint}
	} else {
		config["outbounds"] = []any{target.outbound}
	}
	return json.Marshal(config)
}
