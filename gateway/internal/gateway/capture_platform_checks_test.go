package gateway

import (
	"errors"
	"strings"
	"testing"
)

func TestCaptureWindowsProxyUsesExactRegistryValues(t *testing.T) {
	for _, test := range []struct{ name, registry, proxy, code string }{
		{"disabled with unrelated flag", "ProxyEnable REG_DWORD 0x0\nOther REG_DWORD 0x1\nProxyServer REG_SZ 127.0.0.1:7897", "", ""},
		{"empty PAC", "ProxyEnable REG_DWORD 0x1\nAutoConfigURL REG_SZ\nProxyServer REG_SZ 127.0.0.1:7897", "http://127.0.0.1:7897", ""},
		{"PAC", "AutoConfigURL REG_SZ https://proxy.example/pac", "", "proxy_auto_config_requires_confirmation"},
		{"protocol-specific", "ProxyEnable REG_DWORD 0x1\nProxyServer REG_SZ http=127.0.0.1:7897;https=127.0.0.1:7898", "http://127.0.0.1:7898", ""},
		{"unrecognized", "ProxyEnable REG_DWORD 0x1\nProxyServer REG_SZ socks=127.0.0.1:7897", "", "proxy_config_requires_confirmation"},
		{"malformed enable", "ProxyEnable REG_DWORD broken", "", "proxy_config_requires_confirmation"},
		{"registry name casing", "proxyenable REG_DWORD 0x1\nproxyserver REG_SZ 127.0.0.1:7897", "http://127.0.0.1:7897", ""},
		{"auto detect flag", "AutoDetect REG_DWORD 0x00000001", "", "proxy_auto_config_requires_confirmation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, code := captureWindowsProxy(test.registry)
			if proxy != test.proxy || code != test.code {
				t.Fatal(proxy, code)
			}
		})
	}
}

func TestCaptureLinuxDependenciesReportActualMissingCondition(t *testing.T) {
	for _, test := range []struct{ installed, display, wayland, code string }{
		{"lsof zenity", ":0", "", "capture_linux_sudo_required"},
		{"sudo zenity", ":0", "", "capture_linux_lsof_required"},
		{"sudo lsof", ":0", "", "capture_linux_authorization_ui_required"},
		{"sudo lsof zenity", "", "", "capture_linux_authorization_display_required"},
		{"sudo lsof zenity", ":0", "", ""},
		{"sudo lsof kdialog", "", "wayland-0", ""},
	} {
		lookup := func(name string) (string, error) {
			if strings.Contains(" "+test.installed+" ", " "+name+" ") {
				return "/fixture/" + name, nil
			}
			return "", errors.New("not found")
		}
		err := captureLinuxDependencies(lookup, test.display, test.wayland)
		if (err == nil) != (test.code == "") || err != nil && err.Error() != test.code {
			t.Fatal(test, err)
		}
	}
}

func TestCaptureBackendDiagnosticsAreBoundedAndNeverReturnRawCredentials(t *testing.T) {
	d := &captureDiagnostics{}
	_, _ = d.Write([]byte(strings.Repeat("x", 20000) + " credential=must-not-escape: cannot open display"))
	if len(d.tail) > 8192 || d.code() != "capture_linux_authorization_display_required" {
		t.Fatal("unbounded or unclassified diagnostics")
	}
	for _, test := range []struct{ message, code string }{
		{"sudo: no password was provided", "capture_authorization_denied"},
		{"libbpf: permission denied", "capture_linux_backend_unavailable"},
		{"secret=must-not-escape unknown error", "capture_authorization_or_start_failed"},
	} {
		d := &captureDiagnostics{}
		_, _ = d.Write([]byte(test.message))
		if d.code() != test.code || strings.Contains(d.code(), "must-not-escape") {
			t.Fatal("diagnostic leaked raw content")
		}
	}
}
