package providers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"issue-desk-worker/internal/process"
)

// FlutterCache is the only part of the image's Flutter SDK that a run may write: Flutter rewrites its engine stamp
// there on every command. It is added to the policy only when the image has Flutter.
var FlutterCache = "/opt/flutter/bin/cache"

// flutterCacheAccess lets a run rewrite only the stamps and lock at the top of the Flutter cache. The SDK, engine
// artifacts and tool snapshot inside it stay read-only, so one run cannot leave a changed tool for the next run.
func flutterCacheAccess() map[string]string {
	entries, err := os.ReadDir(FlutterCache)
	if err != nil {
		return nil
	}
	access := map[string]string{FlutterCache: "write"}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".snapshot") {
			access[filepath.Join(FlutterCache, entry.Name())] = "read"
		}
	}
	return access
}

// JavaHome is the image's JDK. process.CleanEnv drops the image ENV, so runs receive it through ToolchainEnv.
var JavaHome = "/opt/jdk"

// ToolchainEnv keeps the JVM and Flutter tool caches in the disposable temp, so HOME stays read-only and nothing
// survives between runs.
func ToolchainEnv(temp string) map[string]string {
	return map[string]string{
		"GRADLE_USER_HOME":  filepath.Join(temp, "gradle"),
		"PUB_CACHE":         filepath.Join(temp, "pub-cache"),
		"JAVA_HOME":         JavaHome,
		"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + temp,
		// The proxy exists only inside the sandbox, after this environment is built.
		"JDK_JAVA_OPTIONS": "\"-javaagent:" + jvmProxyAgent(temp) + "\"",
		// A Gradle or Kotlin daemon would outlive the run and write into the removed temp.
		"GRADLE_OPTS":                "-Dorg.gradle.daemon=false -Dorg.gradle.project.kotlin.compiler.execution.strategy=in-process",
		"FLUTTER_SUPPRESS_ANALYTICS": "true",
	}
}

// Compile once per Worker process, using only the image JDK and no dependencies.
// Each run receives its own jar; a preparation failure leaves an unavailable agent
// path so JVM startup fails closed instead of silently losing proxy enforcement.
var jvmAgentCache struct {
	sync.Mutex
	home string
	jar  []byte
}

func jvmProxyAgent(temp string) string {
	// Never follow a pre-existing jar symlink in a writable run directory.
	runDir, err := os.MkdirTemp(temp, "jvm-proxy-")
	if err != nil {
		return filepath.Join(temp, "jvm-proxy-unavailable-"+UUID()+".jar")
	}
	jar := filepath.Join(runDir, "agent.jar")
	jvmAgentCache.Lock()
	defer jvmAgentCache.Unlock()
	if jvmAgentCache.home != JavaHome || len(jvmAgentCache.jar) == 0 {
		dir, err := os.MkdirTemp(runDir, "build-")
		if err != nil {
			return jar
		}
		defer os.RemoveAll(dir)
		source := filepath.Join(dir, "SandboxProxy.java")
		if os.WriteFile(source, []byte(jvmProxySource), 0600) != nil {
			return jar
		}
		cmd := exec.Command(filepath.Join(JavaHome, "bin", "javac"), "--release", "11", "-d", dir, source)
		cmd.Env = process.CleanEnv(map[string]string{"TMPDIR": dir, "JAVA_TOOL_OPTIONS": "\"-Djava.io.tmpdir=" + dir + "\""})
		if cmd.Run() != nil {
			return jar
		}
		class, err := os.ReadFile(filepath.Join(dir, "SandboxProxy.class"))
		if err != nil {
			return jar
		}
		var data bytes.Buffer
		archive := zip.NewWriter(&data)
		for name, content := range map[string][]byte{
			"META-INF/MANIFEST.MF": []byte("Manifest-Version: 1.0\r\nPremain-Class: SandboxProxy\r\n\r\n"),
			"SandboxProxy.class":   class,
		} {
			file, err := archive.Create(name)
			if err != nil {
				return jar
			}
			if _, err = file.Write(content); err != nil {
				return jar
			}
		}
		if archive.Close() != nil {
			return jar
		}
		jvmAgentCache.home, jvmAgentCache.jar = JavaHome, data.Bytes()
	}
	if os.WriteFile(jar, jvmAgentCache.jar, 0600) != nil {
		return filepath.Join(runDir, "unavailable-"+UUID()+".jar")
	}
	return jar
}

// JAVA_TOOL_OPTIONS remains available for the disposable JVM temp. JDK_JAVA_OPTIONS
// loads this agent in launchers, including Gradle workers and Kotlin JVM children.
// Do not log proxy values: malformed URLs may contain credentials.
const jvmProxySource = `import java.lang.instrument.Instrumentation;
import java.net.*;
import java.io.IOException;
import java.util.List;

public final class SandboxProxy extends ProxySelector {
    private final Proxy http;
    private final Proxy https;
    private SandboxProxy(Proxy http, Proxy https) { this.http = http; this.https = https; }
    private static String env(String name) {
        String value = System.getenv(name);
        if (value == null || value.isEmpty()) value = System.getenv(name.toLowerCase(java.util.Locale.ROOT));
        return value;
    }
    private static Proxy configure(String scheme, String value) throws Exception {
        if (value == null || value.isEmpty()) throw new Exception();
        URI uri = new URI(value);
        int port = uri.getPort() == -1 ? 80 : uri.getPort();
        if (!"http".equalsIgnoreCase(uri.getScheme()) || uri.getHost() == null ||
            uri.getUserInfo() != null || uri.getRawQuery() != null || uri.getRawFragment() != null ||
            !(uri.getRawPath().isEmpty() || uri.getRawPath().equals("/")) || port < 1 || port > 65535)
            throw new Exception();
        String host = uri.getHost();
        System.setProperty(scheme + ".proxyHost", host);
        System.setProperty(scheme + ".proxyPort", Integer.toString(port));
        System.setProperty(scheme + ".nonProxyHosts", "");
        return new Proxy(Proxy.Type.HTTP, new InetSocketAddress(host, port));
    }
    public static void premain(String args, Instrumentation instrumentation) {
        try {
            String http = env("HTTP_PROXY"), https = env("HTTPS_PROXY");
            if (http == null || http.isEmpty()) http = https;
            if (https == null || https.isEmpty()) https = http;
            Proxy httpProxy = configure("http", http), httpsProxy = configure("https", https);
            System.setProperty("java.net.useSystemProxies", "false");
            ProxySelector.setDefault(new SandboxProxy(httpProxy, httpsProxy));
        } catch (Exception invalid) {
            throw new IllegalStateException("Sandbox JVM proxy configuration unavailable");
        }
    }
    private static boolean loopback(String host) {
        if (host == null) return false;
        if (host.equalsIgnoreCase("localhost")) return true;
        if (host.startsWith("[") && host.endsWith("]")) host = host.substring(1, host.length() - 1);
        if (host.indexOf(':') >= 0 && host.matches("[0-9a-fA-F:.]+")) {
            try {
                InetAddress address = InetAddress.getByName(host);
                return address instanceof Inet6Address && address.isLoopbackAddress();
            } catch (UnknownHostException invalid) { return false; }
        }
        String[] parts = host.split("\\.", -1);
        if (parts.length != 4 || !parts[0].equals("127")) return false;
        for (String part : parts) {
            if (!part.matches("0|[1-9][0-9]{0,2}") || Integer.parseInt(part) > 255) return false;
        }
        return true;
    }
    public List<Proxy> select(URI uri) {
        if (uri == null) throw new IllegalArgumentException();
        if (loopback(uri.getHost())) return List.of(Proxy.NO_PROXY);
        return List.of("https".equalsIgnoreCase(uri.getScheme()) ? https : http);
    }
    public void connectFailed(URI uri, SocketAddress address, IOException failure) { }
}
`

type Policy struct {
	Root, Tree, TokenFile string
	Tools                 []string
	ReadFiles             []string
	ReadOnly              bool
	Temp                  string
}

func protected(p Policy) []string {
	dirs := []string{filepath.Dir(p.TokenFile), filepath.Join(p.Root, "state"), filepath.Join(p.Root, "control")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, home)
	}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if value := os.Getenv(key); value != "" {
			dirs = append(dirs, value)
		}
	}
	return dirs
}
func quoted(v any) string { b, _ := json.Marshal(v); return string(b) }
func toml(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, k := range keys {
			parts = append(parts, quoted(k)+"="+toml(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := []string{}
		for _, item := range x {
			parts = append(parts, toml(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return quoted(x)
	}
}
func (p Policy) Args() []string {
	values := map[string]any{"approval_policy": "never", "default_permissions": "gitea-worker", "web_search": "disabled", "features.network_proxy": true, "features.multi_agent": false, "features.apps": false, "features.plugins": false, "features.hooks": false, "features.computer_use": false, "features.browser_use": false, "features.in_app_browser": false, "features.workspace_dependencies": false, "features.skill_search": false, "features.skip_host_skill_discovery": true, "features.unbounded_connection_retries": false}
	filesystem := map[string]any{":root": "read", ":minimal": "read"}
	for _, dir := range protected(p) {
		filesystem[dir] = "deny"
	}
	for _, tool := range p.Tools {
		if tool != "" {
			filesystem[filepath.Dir(tool)] = "read"
		}
	}
	for _, file := range p.ReadFiles {
		filesystem[file] = "read"
	}
	filesystem[filepath.Join(p.Tree, ".git")] = "deny"
	extends := ":workspace"
	if p.ReadOnly {
		extends = ":read-only"
		filesystem[p.Tree] = "read"
	} else {
		if p.Temp != "" {
			filesystem[p.Temp] = "write"
		}
		for path, access := range flutterCacheAccess() {
			filesystem[path] = access
		}
	}
	domains := map[string]any{}
	if !p.ReadOnly {
		for _, domain := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "repo.maven.apache.org", "pub.dev"} {
			domains[domain] = "allow"
		}
	}
	network := map[string]any{"enabled": true, "domains": domains}
	if !p.ReadOnly {
		network["allow_local_binding"] = true
	}
	profile := map[string]any{"extends": extends, "filesystem": filesystem, "network": network}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{}
	for _, key := range keys {
		args = append(args, "-c", key+"="+toml(values[key]))
	}
	return append(args, "-c", "permissions="+toml(map[string]any{"gitea-worker": profile}))
}
func (p Policy) ClaudeSettings() string {
	deny := []string{"Bash", "WebFetch", "WebSearch", "Task", "Agent", "mcp__*", "Read(//proc/**)"}
	if p.ReadOnly {
		deny = append(deny, "Edit", "Write", "NotebookEdit")
	}
	for _, dir := range protected(p) {
		for _, tool := range []string{"Read", "Edit", "Write"} {
			deny = append(deny, tool+"(/"+filepath.ToSlash(dir)+"/**)")
		}
	}
	for _, file := range p.ReadFiles {
		deny = append(deny, "Edit(/"+filepath.ToSlash(file)+")", "Write(/"+filepath.ToSlash(file)+")")
	}
	for _, tool := range []string{"Read", "Edit", "Write"} {
		deny = append(deny, tool+"(/"+filepath.ToSlash(filepath.Join(p.Tree, ".git"))+"/**)")
	}
	return quoted(map[string]any{"permissions": map[string]any{"deny": deny}})
}
