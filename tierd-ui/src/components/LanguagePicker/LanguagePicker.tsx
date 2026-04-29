import { useI18n } from '@rakuensoftware/smoothgui';
import { SUPPORTED_LANGUAGES } from '../../i18n';

// Phase 2 of the SmoothNAS i18n proposal: header language picker.
//
// Renders a small <select> driving SmoothGUI's I18nProvider when at
// least two languages are available. With only English installed
// (Phase 1 / 2 state) the component renders nothing — a single-
// option picker would be UX noise without functional value, and
// an empty header slot is cleaner than a static "English" badge.
//
// When a second language lands (Phase 3 = nl), the same component
// automatically surfaces the picker; no wire change is required at
// any call site.
export default function LanguagePicker() {
  const { language, setLanguage, t } = useI18n();

  if (SUPPORTED_LANGUAGES.length < 2) return null;

  return (
    <label
      style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: '#666' }}
      title={t('topbar.language.tooltip', undefined, 'Interface language')}
    >
      <span style={{ userSelect: 'none' }}>
        {t('topbar.language.label', undefined, 'Language')}
      </span>
      <select
        value={language}
        onChange={(e) => setLanguage(e.target.value)}
        aria-label={t('topbar.language.label', undefined, 'Language')}
      >
        {SUPPORTED_LANGUAGES.map((lang) => (
          <option key={lang.code} value={lang.code}>{lang.label}</option>
        ))}
      </select>
    </label>
  );
}
