package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/osuki-dev/kizuna/locales"
)

var (
	currentLang = "en"
	mu          sync.RWMutex
	messages    = make(map[string]map[string]string)
)

func init() {
	ReloadLocales()
	currentLang = DetectSystemLanguage()
}

// ReloadLocales reloads embedded locales and scans external directories for user-defined translation files
func ReloadLocales() {
	mu.Lock()
	defer mu.Unlock()

	// 1. Load embedded default locales
	entries, err := locales.Content.ReadDir(".")
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				lang := strings.TrimSuffix(entry.Name(), ".json")
				data, err := locales.Content.ReadFile(entry.Name())
				if err == nil {
					var dict map[string]string
					if err := json.Unmarshal(data, &dict); err == nil {
						if messages[lang] == nil {
							messages[lang] = make(map[string]string)
						}
						for k, v := range dict {
							messages[lang][k] = v
						}
					}
				}
			}
		}
	}

	// 2. Discover external translation directories: ./locales, ~/.kizuna/locales, KIZUNA_LOCALES_DIR
	var searchDirs []string
	if customDir := os.Getenv("KIZUNA_LOCALES_DIR"); customDir != "" {
		searchDirs = append(searchDirs, customDir)
	}
	searchDirs = append(searchDirs, "locales", "./locales")

	if home, err := os.UserHomeDir(); err == nil {
		searchDirs = append(searchDirs, filepath.Join(home, ".kizuna", "locales"))
	}

	for _, dir := range searchDirs {
		loadFromDir(dir)
	}
}

func loadFromDir(dirPath string) {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		ext := filepath.Ext(f.Name())
		if ext != ".json" {
			continue
		}
		lang := strings.TrimSuffix(f.Name(), ext)
		lang = strings.ToLower(strings.TrimSpace(lang))

		fullPath := filepath.Join(dirPath, f.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		var dict map[string]string
		if err := json.Unmarshal(data, &dict); err == nil {
			if messages[lang] == nil {
				messages[lang] = make(map[string]string)
			}
			for k, v := range dict {
				messages[lang][k] = v
			}
		}
	}
}

// matchLanguageCandidate searches registered languages dynamically for the best match
func matchLanguageCandidate(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	// Strip encoding like ".utf-8" or ".utf8"
	if idx := strings.Index(raw, "."); idx != -1 {
		raw = raw[:idx]
	}

	// 1. Direct match (e.g. "ja", "zh", "en", "fr", "de")
	if _, ok := messages[raw]; ok {
		return raw
	}

	// 2. Base language prefix match (e.g. "zh_CN" -> "zh", "fr_FR" -> "fr", "pt-BR" -> "pt")
	base := raw
	for _, sep := range []string{"_", "-"} {
		if parts := strings.Split(raw, sep); len(parts) > 1 {
			base = parts[0]
			break
		}
	}
	if _, ok := messages[base]; ok {
		return base
	}

	// 3. Prefix match against any registered locale (e.g. "zh" matches "zh-cn" if registered)
	for regLang := range messages {
		if strings.HasPrefix(regLang, base) || strings.HasPrefix(base, regLang) {
			return regLang
		}
	}

	return ""
}

// DetectSystemLanguage detects the appropriate language code from environment variables
func DetectSystemLanguage() string {
	mu.RLock()
	defer mu.RUnlock()

	for _, envKey := range []string{"KIZUNA_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		val := os.Getenv(envKey)
		if val == "" {
			continue
		}
		if match := matchLanguageCandidate(val); match != "" {
			return match
		}
	}
	return "en"
}

// SetLanguage sets current active language code dynamically
func SetLanguage(lang string) {
	mu.Lock()
	defer mu.Unlock()

	if match := matchLanguageCandidate(lang); match != "" {
		currentLang = match
		return
	}
	currentLang = "en"
}

// GetLanguage returns the current active language code
func GetLanguage() string {
	mu.RLock()
	defer mu.RUnlock()
	return currentLang
}

// AvailableLanguages returns sorted list of all registered language codes
func AvailableLanguages() []string {
	mu.RLock()
	defer mu.RUnlock()
	list := make([]string, 0, len(messages))
	for k := range messages {
		list = append(list, k)
	}
	sort.Strings(list)
	return list
}

// RegisterLanguage registers or extends translations for a language code
func RegisterLanguage(code string, dict map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	code = strings.ToLower(strings.TrimSpace(code))
	if messages[code] == nil {
		messages[code] = make(map[string]string)
	}
	for k, v := range dict {
		messages[code][k] = v
	}
}

// T formats a translated string with arguments using the active language, falling back to 'en'
func T(key string, args ...any) string {
	mu.RLock()
	defer mu.RUnlock()

	dict, ok := messages[currentLang]
	if !ok {
		dict = messages["en"]
	}
	tmpl, ok := dict[key]
	if !ok || tmpl == "" {
		if enDict, okEn := messages["en"]; okEn {
			tmpl = enDict[key]
		}
		if tmpl == "" {
			tmpl = key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(tmpl, args...)
	}
	return tmpl
}

// Has checks whether a translation key is defined in active language or fallback 'en'
func Has(key string) bool {
	mu.RLock()
	defer mu.RUnlock()
	if dict, ok := messages[currentLang]; ok {
		if val, exists := dict[key]; exists && val != "" {
			return true
		}
	}
	if enDict, ok := messages["en"]; ok {
		if val, exists := enDict[key]; exists && val != "" {
			return true
		}
	}
	return false
}

