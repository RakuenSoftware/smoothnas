import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@rakuensoftware/smoothgui/styles';
import './styles.scss';
import '@xterm/xterm/css/xterm.css';
import App from './App';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>
);
