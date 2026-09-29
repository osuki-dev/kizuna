package i18n_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
)

func TestI18nLanguages(t *testing.T) {
	// 1. Test English
	i18n.SetLanguage("en")
	if i18n.GetLanguage() != "en" {
		t.Fatalf("expected lang en, got %s", i18n.GetLanguage())
	}
	enDeploy := i18n.T("deploy_starting", "web", "docker", "srv1")
	if enDeploy != "Deploying service 'web' (type: docker) to srv1..." {
		t.Errorf("unexpected en deploy translation: %s", enDeploy)
	}

	// 2. Test Japanese
	i18n.SetLanguage("ja")
	if i18n.GetLanguage() != "ja" {
		t.Fatalf("expected lang ja, got %s", i18n.GetLanguage())
	}
	jaDeploy := i18n.T("deploy_starting", "web", "docker", "srv1")
	if jaDeploy != "サービス 'web' (種類: docker) を srv1 にデプロイしています..." {
		t.Errorf("unexpected ja deploy translation: %s", jaDeploy)
	}

	jaAppDesc := i18n.T("app_desc")
	if jaAppDesc != "絆 (Kizuna) - ゼロトラストメッシュデプロイ＆Webサイト公開CLI" {
		t.Errorf("unexpected ja app_desc: %s", jaAppDesc)
	}

	// 3. Test Chinese
	i18n.SetLanguage("zh")
	if i18n.GetLanguage() != "zh" {
		t.Fatalf("expected lang zh, got %s", i18n.GetLanguage())
	}
	zhDeploy := i18n.T("deploy_starting", "web", "docker", "srv1")
	if zhDeploy != "开始部署服务 'web' (类型: docker) 到 srv1..." {
		t.Errorf("unexpected zh deploy translation: %s", zhDeploy)
	}

	// 4. Test Fallback for missing key
	i18n.SetLanguage("ja")
	i18n.RegisterLanguage("en", map[string]string{"only_in_en": "English only value: %s"})
	val := i18n.T("only_in_en", "test")
	if val != "English only value: test" {
		t.Errorf("expected fallback to en, got %s", val)
	}

	// Non-existent key returns key itself
	nonExistent := i18n.T("non_existent_key_12345")
	if nonExistent != "non_existent_key_12345" {
		t.Errorf("expected key itself, got %s", nonExistent)
	}
}

func TestExternalLocaleFileDiscovery(t *testing.T) {
	// Verify adding a new JSON file to a directory allows loading without recompiling code!
	tmpDir := t.TempDir()
	frJSON := `{
		"welcome": "Bienvenue sur Kizuna, %s !",
		"goodbye": "Au revoir"
	}`
	if err := os.WriteFile(filepath.Join(tmpDir, "fr.json"), []byte(frJSON), 0644); err != nil {
		t.Fatalf("failed to write fr.json: %v", err)
	}

	origLocalesDir := os.Getenv("KIZUNA_LOCALES_DIR")
	defer func() {
		_ = os.Setenv("KIZUNA_LOCALES_DIR", origLocalesDir)
		i18n.ReloadLocales()
	}()

	_ = os.Setenv("KIZUNA_LOCALES_DIR", tmpDir)
	i18n.ReloadLocales()

	i18n.SetLanguage("fr")
	if i18n.GetLanguage() != "fr" {
		t.Fatalf("expected language fr, got %s", i18n.GetLanguage())
	}

	msg := i18n.T("welcome", "Alice")
	if msg != "Bienvenue sur Kizuna, Alice !" {
		t.Errorf("unexpected French message: %s", msg)
	}
}

func TestDynamicLanguageRegistration(t *testing.T) {
	customDict := map[string]string{
		"welcome": "Herzlich Willkommen %s",
	}
	i18n.RegisterLanguage("de", customDict)
	i18n.SetLanguage("de")

	out := i18n.T("welcome", "Kizuna")
	if out != "Herzlich Willkommen Kizuna" {
		t.Errorf("expected German greeting, got %s", out)
	}

	langs := i18n.AvailableLanguages()
	foundDe := false
	for _, l := range langs {
		if l == "de" {
			foundDe = true
			break
		}
	}
	if !foundDe {
		t.Errorf("expected 'de' in available languages list: %v", langs)
	}
}

func TestSystemLanguageDetection(t *testing.T) {
	origLang := os.Getenv("LANG")
	origKizunaLang := os.Getenv("KIZUNA_LANG")
	defer func() {
		_ = os.Setenv("LANG", origLang)
		_ = os.Setenv("KIZUNA_LANG", origKizunaLang)
	}()

	_ = os.Unsetenv("KIZUNA_LANG")
	_ = os.Setenv("LANG", "ja_JP.UTF-8")
	if detected := i18n.DetectSystemLanguage(); detected != "ja" {
		t.Errorf("expected ja detection from ja_JP.UTF-8, got %s", detected)
	}

	_ = os.Setenv("LANG", "en_US.UTF-8")
	if detected := i18n.DetectSystemLanguage(); detected != "en" {
		t.Errorf("expected en detection from en_US.UTF-8, got %s", detected)
	}

	_ = os.Setenv("LANG", "zh_CN.UTF-8")
	if detected := i18n.DetectSystemLanguage(); detected != "zh" {
		t.Errorf("expected zh detection from zh_CN.UTF-8, got %s", detected)
	}

	// KIZUNA_LANG takes precedence over LANG
	_ = os.Setenv("KIZUNA_LANG", "ja")
	_ = os.Setenv("LANG", "en_US.UTF-8")
	if detected := i18n.DetectSystemLanguage(); detected != "ja" {
		t.Errorf("expected ja precedence from KIZUNA_LANG, got %s", detected)
	}

	// Completely dynamic detection for newly registered language
	_ = os.Setenv("KIZUNA_LANG", "")
	_ = os.Setenv("LANG", "de_DE.UTF-8")
	i18n.RegisterLanguage("de", map[string]string{"k": "v"})
	if detected := i18n.DetectSystemLanguage(); detected != "de" {
		t.Errorf("expected dynamic de detection from de_DE.UTF-8, got %s", detected)
	}
}
