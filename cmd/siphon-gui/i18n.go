package main

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2/lang"
)

// The window speaks English, Turkish or German. The English text is the
// key: T returns it as is in English and its translation otherwise, so the
// code reads in English and English is never incomplete. The catalogs are
// in translations_tr.go and translations_de.go; a test checks that every
// text the code asks for has both translations, with the same verbs.
//
// Only the window is translated. Errors that come from the sites and the
// downloader ("HTTP 404: …") and the Diagnose report stay in English: they
// are technical, and a search for them should find the same words.

type language string

const (
	langAuto    language = "" // the system's, if Siphon speaks it
	langEnglish language = "en"
	langTurkish language = "tr"
	langGerman  language = "de"
)

// prefLanguage is the preference holding the chosen language.
const prefLanguage = "language"

// languageChoice is one entry of the Settings list; each language is named
// in itself, so it can be found whatever the window speaks now.
type languageChoice struct {
	code language
	name string
}

func languageChoices() []languageChoice {
	return []languageChoice{
		{langAuto, T("Automatic (system language)")},
		{langEnglish, "English"},
		{langTurkish, "Türkçe"},
		{langGerman, "Deutsch"},
	}
}

var catalogs = map[language]map[string]string{
	langTurkish: turkish,
	langGerman:  german,
}

// current is the language the window speaks. It is set once at start,
// before any window is built; a change takes effect at the next start.
var current = langEnglish

// setLanguage picks the language to speak for a choice.
func setLanguage(choice language) {
	current = resolveLanguage(choice, systemLanguage())
}

// resolveLanguage: a language Siphon speaks is itself; Automatic is the
// system's language when Siphon speaks it, English otherwise.
func resolveLanguage(choice, system language) language {
	if choice == langAuto {
		choice = system
	}
	if choice == langEnglish || catalogs[choice] != nil {
		return choice
	}
	return langEnglish
}

func systemLanguage() language {
	return language(strings.ToLower(lang.SystemLocale().LanguageString()))
}

// T translates an English text into the current language.
func T(s string) string {
	if t, ok := catalogs[current][s]; ok && t != "" {
		return t
	}
	return s
}

// Tf translates a format, then fills it in.
func Tf(format string, a ...any) string {
	return fmt.Sprintf(T(format), a...)
}

// msg marks a text for translation where it is written down (a table of
// labels, say) but translated later, with T, where it is shown.
func msg(s string) string { return s }
