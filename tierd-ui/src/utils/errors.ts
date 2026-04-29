// Re-export the error helpers from SmoothGUI so SmoothNAS pages
// can import from a single local barrel:
//
//   import { extractError, useExtractError } from '../../utils/errors';
//
// Both are i18n-aware as of SmoothGUI 0.4.1: useExtractError binds
// to the active I18nProvider and looks up `error.<code>` keys for
// any thrown ApiError before falling back to the server's English
// message. Local re-exports avoid scattering @rakuensoftware/smoothgui
// imports through every page touch site.
export { extractError, useExtractError } from '@rakuensoftware/smoothgui';
