package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"issue-desk-worker/internal/process"
)

func TestReviewContractRejectsMissingFieldsAndExtraJSON(t *testing.T) {
	valid := `{"decision":"approve","summary":"確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	if err := Validate([]byte(valid), true); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"userJudgments":[],`, `,"resolvableUncertainties":[]`} {
		if err := Validate([]byte(strings.Replace(valid, field, "", 1)), true); err == nil {
			t.Fatalf("必須の分類がありません: %s", field)
		}
	}
	if err := Validate([]byte(valid+`{}`), true); err == nil {
		t.Fatal("追加のJSONを受理しました")
	}
	if err := Validate([]byte(strings.Replace(valid, `"breaking":false,`, "", 1)), true); err == nil {
		t.Fatal("必須の破壊的変更判定がありません")
	}
}
func TestPolicyDeniesSecretsAndReviewWrites(t *testing.T) {
	policy := Policy{Root: "/state/runtime", Tree: "/state/runtime/workspaces/job", TokenFile: "/state/private/worker-token", Tools: []string{"/opt/bin/codex"}, ReadOnly: true}
	args := strings.Join(policy.Args(), " ")
	for _, expected := range []string{`"/state/private"="deny"`, `"/state/runtime/state"="deny"`, `"/state/runtime/control"="deny"`, `"/state/runtime/workspaces/job/.git"="deny"`, `":read-only"`, `"domains"={}`} {
		if !strings.Contains(args, expected) {
			t.Error("隔離方針に必要な拒否がありません", expected)
		}
	}
	if strings.Contains(args, `"approval_policy"="always"`) {
		t.Fatal("承認を広げました")
	}
}
func TestPolicyAllowsJVMAndFlutterOnlyForWritableRuns(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "flutter", "bin", "cache")
	for _, dir := range []string{"dart-sdk/bin", "artifacts/engine"} {
		if err := os.MkdirAll(filepath.Join(cache, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"flutter_tools.snapshot", "engine.stamp"} {
		if err := os.WriteFile(filepath.Join(cache, file), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	saved := FlutterCache
	FlutterCache = cache
	t.Cleanup(func() { FlutterCache = saved })
	base := Policy{Root: "/state/runtime", Tree: "/state/runtime/workspaces/job", TokenFile: "/state/private/worker-token", Temp: "/state/runtime/tmp/job"}
	writable := strings.Join(base.Args(), " ")
	// Check the entire generated allow set, not just the domains needed by JVMs.
	allowed := regexp.MustCompile(`"([a-z0-9.-]+)"="allow"`)
	wantDomains := []string{"files.pythonhosted.org", "proxy.golang.org", "pub.dev", "pypi.org", "registry.npmjs.org", "repo.maven.apache.org", "storage.googleapis.com", "sum.golang.org"}
	var domains []string
	for _, match := range allowed.FindAllStringSubmatch(writable, -1) {
		domains = append(domains, match[1])
	}
	if !reflect.DeepEqual(domains, wantDomains) {
		t.Fatalf("通信先集合が変わりました: %v", domains)
	}
	for _, expected := range []string{`"repo.maven.apache.org"="allow"`, `"pub.dev"="allow"`, `"storage.googleapis.com"="allow"`, quoted(cache) + `="write"`, `"/state/runtime/tmp/job"="write"`} {
		if !strings.Contains(writable, expected) {
			t.Error("書き込みのある実行に必要な許可がありません", expected)
		}
	}
	for _, name := range []string{"dart-sdk", "artifacts", "flutter_tools.snapshot"} {
		if !strings.Contains(writable, quoted(filepath.Join(cache, name))+`="read"`) {
			t.Error("Flutter の道具本体を書き込み可にしました", name)
		}
	}
	if strings.Contains(writable, quoted(filepath.Join(cache, "engine.stamp"))) {
		t.Error("印のファイルを読み取りだけにしました")
	}
	if strings.Contains(writable, `"/opt/flutter"="write"`) || strings.Contains(writable, quoted(filepath.Dir(cache))+`="write"`) {
		t.Fatal("Flutter SDK 全体へ書き込みを広げました")
	}
	readOnly := base
	readOnly.ReadOnly = true
	args := strings.Join(readOnly.Args(), " ")
	if domains := allowed.FindAllStringSubmatch(args, -1); len(domains) != 0 {
		t.Fatalf("読取専用の許可通信先集合が変わりました: %v", domains)
	}
	if strings.Contains(args, "repo.maven.apache.org") || strings.Contains(args, "pub.dev") || strings.Contains(args, quoted(cache)+`="write"`) {
		t.Fatal("読み取りだけの実行に通信先か書き込み先を足しました")
	}
	FlutterCache = filepath.Join(t.TempDir(), "missing")
	if strings.Contains(strings.Join(base.Args(), " "), quoted(FlutterCache)) {
		t.Fatal("Flutter の無いイメージに書き込み先を足しました")
	}
}

func TestToolchainEnvKeepsCachesInTemp(t *testing.T) {
	temp := filepath.Join("work", "tmp")
	env := ToolchainEnv(temp)
	if env["GRADLE_USER_HOME"] != filepath.Join(temp, "gradle") || env["PUB_CACHE"] != filepath.Join(temp, "pub-cache") {
		t.Fatalf("道具のキャッシュが一時領域の外です: %v", env)
	}
	if env["JAVA_HOME"] != JavaHome || env["JAVA_TOOL_OPTIONS"] != "-Djava.io.tmpdir="+temp || env["FLUTTER_SUPPRESS_ANALYTICS"] != "true" || !strings.Contains(env["GRADLE_OPTS"], "-Dorg.gradle.daemon=false") {
		t.Fatalf("JDK と Flutter の設定がありません: %v", env)
	}
}

func TestMeasuredZeroUsageRemainsZero(t *testing.T) {
	usage := readUsage(json.RawMessage(`{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}`), "codex")
	if usage.Input == nil || usage.Output == nil || *usage.Input != 0 || *usage.Output != 0 {
		t.Fatal("実測ゼロを欠測に変えました")
	}
	missing := readUsage(json.RawMessage(`{}`), "claude")
	if missing.Input != nil || missing.Output != nil {
		t.Fatal("欠測をゼロとみなしました")
	}
}

func TestClaudeResultCost(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        *float64
	}{
		{"value", `,"total_cost_usd":1.25`, costPtr(1.25)},
		{"zero", `,"total_cost_usd":0`, costPtr(0)},
		{"missing", ``, nil}, {"null", `,"total_cost_usd":null`, nil},
		{"negative", `,"total_cost_usd":-1`, nil}, {"string", `,"total_cost_usd":"1"`, nil},
		{"overflow", `,"total_cost_usd":1e999`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "claude")
			stream := `{"type":"assistant","usage":{"input_tokens":1}}` + "\n" + `{"type":"result","usage":{"input_tokens":2},"num_turns":1` + tc.field + `}` + "\n" + `{"type":"assistant","usage":{"input_tokens":3}}`
			if err := os.WriteFile(exe, []byte("#!/bin/sh\ncat <<'FIXTURE'\n"+stream+"\nFIXTURE\n"), 0700); err != nil {
				t.Fatal(err)
			}
			answer, _ := Call(context.Background(), Request{Settings: Settings{Provider: "claude"}, Policy: Policy{Root: dir, Tree: dir}, Claude: exe})
			got := answer.Usage.CostUSD
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("cost: %v want %v", got, tc.want)
			}
		})
	}
}
func costPtr(v float64) *float64 { return &v }

func TestJVMRuntimeProxyAndChildren(t *testing.T) {
	var direct atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		fmt.Fprint(w, "direct")
	}))
	defer origin.Close()
	tlsDirect := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	tlsDirect.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			direct.Add(1)
		}
	}
	tlsDirect.StartTLS()
	defer tlsDirect.Close()
	tlsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "proxied")
	}))
	defer tlsOrigin.Close()
	useTestJDK(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "ProxyProbe.java")
	if err := os.WriteFile(source, []byte(jvmProxyProbe), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := process.Run(context.Background(), filepath.Join(JavaHome, "bin", "javac"), []string{"-d", dir, source}, process.Options{})
	if err != nil || out.Code != 0 {
		t.Fatalf("JVM fixture compilation: %v %+v", err, out)
	}
	for _, name := range []string{"upper-separated", "lower-https-only", "upper-http-only", "deny", "unreachable", "invalid", "missing", "prepare-failed"} {
		t.Run(name, func(t *testing.T) {
			var requests, connects atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if name == "deny" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				fmt.Fprint(w, "proxied")
			}))
			defer proxy.Close()
			tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					requests.Add(1)
					if name == "deny" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					fmt.Fprint(w, "proxied")
					return
				}
				connects.Add(1)
				if name == "deny" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				upstream, err := net.Dial("tcp", tlsOrigin.Listener.Addr().String())
				if err != nil {
					t.Error(err)
					w.WriteHeader(502)
					return
				}
				defer upstream.Close()
				client, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer client.Close()
				fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(upstream, buffered) }()
				_, _ = io.Copy(client, upstream)
			}))
			defer tunnel.Close()
			temp := t.TempDir()
			workDir := temp
			if name == "prepare-failed" {
				temp = filepath.Join(temp, "missing")
			}
			// Build on the host first. Only the child receives the runtime proxy.
			env := ToolchainEnv(temp)
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
				env[key] = ""
			}
			env["HTTP_PROXY"], env["HTTPS_PROXY"] = proxy.URL, tunnel.URL
			env["NO_PROXY"], env["no_proxy"] = "*", "*"
			// Explicit bypass properties must not permit external destinations.
			env["JAVA_TOOL_OPTIONS"] += " -Dhttp.nonProxyHosts=* -Dhttps.nonProxyHosts=*"
			mode := "ok"
			switch name {
			case "lower-https-only":
				env["HTTP_PROXY"], env["HTTPS_PROXY"], env["https_proxy"] = "", "", tunnel.URL
			case "upper-http-only":
				env["HTTP_PROXY"], env["HTTPS_PROXY"] = tunnel.URL, ""
			case "deny":
				mode = "denied"
			case "unreachable":
				proxy.Close()
				tunnel.Close()
				mode = "denied"
			case "invalid":
				env["HTTPS_PROXY"] = "http://user:fixture-secret@127.0.0.1:1"
			case "missing":
				env["HTTP_PROXY"], env["HTTPS_PROXY"] = "", ""
			}
			args := []string{"-cp", dir, "ProxyProbe", mode, strings.Replace(origin.URL, "127.0.0.1", "external.example", 1), strings.Replace(tlsDirect.URL, "127.0.0.1", "external.example", 1), temp, origin.URL}
			out, err := process.Run(context.Background(), filepath.Join(JavaHome, "bin", "java"), args, process.Options{Dir: workDir, Env: env})
			if name == "invalid" || name == "missing" || name == "prepare-failed" {
				if err != nil || out.Code == 0 || strings.Contains(out.Out+out.Err, "fixture-secret") {
					t.Fatalf("invalid proxy was accepted or leaked: code=%d error=%v", out.Code, err)
				}
			} else if err != nil || out.Code != 0 || !strings.Contains(out.Out, "child-ok") {
				t.Fatalf("JVM proxy: %v code=%d\n%s\n%s", err, out.Code, out.Out, out.Err)
			}
			wantDirect := int32(2)
			if name == "invalid" || name == "missing" || name == "prepare-failed" {
				wantDirect = 0
			}
			if got := direct.Swap(0); got != wantDirect {
				t.Fatalf("direct loopback requests: got %d, want %d", got, wantDirect)
			}
			if name == "upper-separated" || name == "lower-https-only" || name == "upper-http-only" || name == "deny" {
				if requests.Load() < 2 || connects.Load() < 2 {
					t.Fatalf("parent/child proxy traffic missing: HTTP=%d CONNECT=%d", requests.Load(), connects.Load())
				}
			}
		})
	}
}

const jvmProxyProbe = `import java.net.*;
import java.nio.file.*;
import javax.net.ssl.*;
public class ProxyProbe {
    public static void main(String[] args) throws Exception {
        String temp = args[3];
        if (!System.getProperty("java.io.tmpdir").equals(temp) ||
            !System.getenv("GRADLE_USER_HOME").equals(temp + "/gradle") ||
            !System.getenv("PUB_CACHE").equals(temp + "/pub-cache") ||
            !System.getenv("GRADLE_OPTS").contains("-Dorg.gradle.daemon=false") ||
            !System.getenv("GRADLE_OPTS").contains("kotlin.compiler.execution.strategy=in-process"))
            throw new AssertionError("temporary cache/daemon settings lost");
        Files.createDirectories(Path.of(System.getenv("GRADLE_USER_HOME")));
        Files.writeString(Path.of(System.getenv("GRADLE_USER_HOME"), "probe"), "cache");
        TrustManager[] trust = { new X509TrustManager() {
            public java.security.cert.X509Certificate[] getAcceptedIssuers() { return new java.security.cert.X509Certificate[0]; }
            public void checkClientTrusted(java.security.cert.X509Certificate[] c, String a) {}
            public void checkServerTrusted(java.security.cert.X509Certificate[] c, String a) {}
        }};
        SSLContext ssl = SSLContext.getInstance("TLS");
        ssl.init(null, trust, null);
        HttpsURLConnection.setDefaultSSLSocketFactory(ssl.getSocketFactory());
        HttpsURLConnection.setDefaultHostnameVerifier((h, s) -> true);
        for (int i = 1; i <= 2; i++) {
            try {
                URLConnection connection = URI.create(args[i]).toURL().openConnection();
                connection.setConnectTimeout(3000);
                connection.setReadTimeout(3000);
                String body = new String(connection.getInputStream().readAllBytes(), java.nio.charset.StandardCharsets.UTF_8);
                if (args[0].equals("denied") || !body.equals("proxied")) throw new AssertionError("proxy bypass");
            } catch (java.io.IOException denied) {
                if (!args[0].equals("denied")) throw denied;
            }
        }
        for (String host : new String[]{"localhost", "LOCALHOST", "127.0.0.1", "127.255.1.2", "[::1]", "[0:0:0:0:0:0:0:1]"}) {
            if (ProxySelector.getDefault().select(URI.create("http://" + host)).get(0) != Proxy.NO_PROXY)
                throw new AssertionError("loopback not direct: " + host);
        }
        for (String host : new String[]{"localhost.example", "127.0.0.1.example", "128.0.0.1", "[::2]"}) {
            if (ProxySelector.getDefault().select(URI.create("http://" + host)).get(0) == Proxy.NO_PROXY)
                throw new AssertionError("external bypass: " + host);
        }
        URLConnection local = URI.create(args[4]).toURL().openConnection();
        local.setConnectTimeout(3000);
        local.setReadTimeout(3000);
        if (!new String(local.getInputStream().readAllBytes(), java.nio.charset.StandardCharsets.UTF_8).equals("direct"))
            throw new AssertionError("loopback connection failed");
        if (args.length == 5) {
            Process child = new ProcessBuilder(System.getProperty("java.home") + "/bin/java", "-cp",
                System.getProperty("java.class.path"), "ProxyProbe", args[0], args[1], args[2], temp, args[4], "child").inheritIO().start();
            if (child.waitFor() != 0) throw new AssertionError("child JVM failed");
        } else System.out.println("child-ok");
    }
}
`

// useTestJDK points JavaHome at a JDK the test can run: the image's /opt/jdk, then
// JAVA_HOME, then the javac on PATH (hosts that verify outside the image). A missing
// JDK fails the test with the places it looked; it is never skipped. It replaces the
// JavaHome global, so tests that call it must not use t.Parallel.
func useTestJDK(t *testing.T) {
	t.Helper()
	candidates := []string{JavaHome}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		candidates = append(candidates, home)
	}
	if javac, err := exec.LookPath("javac"); err == nil {
		if real, err := filepath.EvalSymlinks(javac); err == nil {
			candidates = append(candidates, filepath.Dir(filepath.Dir(real)))
		}
	}
	for _, home := range candidates {
		if info, err := os.Stat(filepath.Join(home, "bin", "javac")); err == nil && !info.IsDir() {
			if _, err := os.Stat(filepath.Join(home, "bin", "java")); err == nil {
				previous := JavaHome
				JavaHome = home
				t.Logf("JDK: %s", home)
				t.Cleanup(func() { JavaHome = previous })
				return
			}
		}
	}
	t.Fatalf("JDK が見つかりません。探した場所: %v（JAVA_HOME と PATH の javac を含む）。deploy/sdk-release.json の jdk を入れてください", candidates)
}
