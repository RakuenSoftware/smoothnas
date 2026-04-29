// Phase 1 of the SmoothNAS i18n proposal:
// docs/proposals/pending/smoothnas-i18n-en-nl.md
//
// SmoothNAS routes its translations through SmoothGUI's I18nProvider
// (added in @rakuensoftware/smoothgui@0.3.0). The provider already
// covers the strings SmoothGUI's own components use (alerts, login,
// confirm dialogs, user dropdown, toasts) — see englishTranslations
// in @rakuensoftware/smoothgui — and lets consumers extend the
// catalog with their own keys.
//
// This module owns the SmoothNAS-side scaffolding:
//
//   - SUPPORTED_LANGUAGES is the source of truth for the language
//     picker. Adding a third language is a translation-only PR:
//     drop the new bundle into `./locales/<code>.ts`, list it here,
//     and the picker picks it up.
//   - smoothnasTranslations is the SmoothNAS-specific catalog. Phase 2
//     replaces JSX literals with t() lookups against these keys; Phase 3
//     adds non-English bundles.
//   - resolveInitialLanguage picks the language at app boot from a
//     query-string override, localStorage, then navigator (Accept-
//     Language). The proposal's "post-login per-user persistence"
//     stitches in once a header picker lands in Phase 2.
//
// Phase 1 is purely additive — every existing JSX literal still
// renders the same English text it always did until Phase 2 keys
// each page through t().

import type { TranslationCatalog } from '@rakuensoftware/smoothgui';

import en from './locales/en';
import nl from './locales/nl';

export const SUPPORTED_LANGUAGES = [
  { code: 'en', label: 'English' },
  { code: 'nl', label: 'Nederlands' },
] as const;

export type LanguageCode = (typeof SUPPORTED_LANGUAGES)[number]['code'];

export const FALLBACK_LANGUAGE: LanguageCode = 'en';

// smoothnasTranslations layers on top of SmoothGUI's built-in
// englishTranslations. Per-key overrides win over SmoothGUI's
// defaults, and SmoothNAS-specific keys (everything else here)
// flow through the same useI18n() consumers will use in Phase 2.
export const smoothnasTranslations: TranslationCatalog = {
  en,
  nl,
};

// resolveInitialLanguage reads the synchronous detection chain:
//
//   1. ?lang=<code>          QA / scripted access; honoured even
//                            when the operator isn't logged in.
//   2. localStorage           the operator's persisted choice from
//      'smoothnas.lang'       the header picker.
//   3. navigator.language     fallback to the browser's
//                             Accept-Language preference.
//   4. FALLBACK_LANGUAGE      last resort.
//
// The chain stops at the first known supported language. The
// installer-chosen system default lives behind an async fetch
// (`fetchSystemLocale`) and is layered on top of this sync result
// by App.tsx — see `hasUserOverride()` for the gate that keeps a
// user's explicit picker choice from being overwritten.
export function resolveInitialLanguage(): LanguageCode {
  if (typeof window === 'undefined') return FALLBACK_LANGUAGE;

  const supported = new Set<string>(SUPPORTED_LANGUAGES.map((l) => l.code));
  const isSupported = (code: string | null | undefined): code is LanguageCode =>
    !!code && supported.has(code);

  const params = new URLSearchParams(window.location.search);
  const fromQuery = params.get('lang');
  if (isSupported(fromQuery)) return fromQuery;

  try {
    const fromStorage = window.localStorage.getItem('smoothnas.lang');
    if (isSupported(fromStorage)) return fromStorage;
  } catch {
    // localStorage may throw on privacy-mode browsers.
  }

  const navLang = navigator.language?.split('-')[0];
  if (isSupported(navLang)) return navLang;

  return FALLBACK_LANGUAGE;
}

// hasUserOverride reports whether the operator has explicitly
// picked a language for this browser (?lang= URL override or a
// previous LanguagePicker choice persisted in localStorage). When
// true, we do NOT overlay the installer's system default — the
// user's explicit pick wins.
export function hasUserOverride(): boolean {
  if (typeof window === 'undefined') return false;

  const supported = new Set<string>(SUPPORTED_LANGUAGES.map((l) => l.code));
  const params = new URLSearchParams(window.location.search);
  const fromQuery = params.get('lang');
  if (fromQuery && supported.has(fromQuery)) return true;

  try {
    const fromStorage = window.localStorage.getItem('smoothnas.lang');
    if (fromStorage && supported.has(fromStorage)) return true;
  } catch {
    // localStorage may throw on privacy-mode browsers.
  }
  return false;
}

// fetchSystemLocale asks the unauthenticated `/api/locale` endpoint
// for the language the installer wrote during firstboot. Returns a
// supported LanguageCode on success, or null if the endpoint is
// unreachable / returns an unsupported tag.
export async function fetchSystemLocale(): Promise<LanguageCode | null> {
  try {
    const resp = await fetch('/api/locale', { credentials: 'same-origin' });
    if (!resp.ok) return null;
    const body = (await resp.json()) as { language?: string };
    const lang = (body.language || '').split('-')[0];
    const supported = new Set<string>(SUPPORTED_LANGUAGES.map((l) => l.code));
    return supported.has(lang) ? (lang as LanguageCode) : null;
  } catch {
    return null;
  }
}

// persistLanguage writes the operator's choice to localStorage so
// the picker decision survives a full page reload (and pre-empts
// Accept-Language on subsequent visits).
export function persistLanguage(lang: LanguageCode): void {
  try {
    window.localStorage.setItem('smoothnas.lang', lang);
  } catch {
    // Privacy-mode browser; the in-memory I18nProvider state still
    // honours the operator's choice for the current session.
  }
}
