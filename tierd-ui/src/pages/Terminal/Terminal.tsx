import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { useI18n } from '@rakuensoftware/smoothgui';
import './Terminal.scss';

export default function Terminal() {
  const { t } = useI18n();
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const resizeRef = useRef<ResizeObserver | null>(null);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    const term = new XTerm({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      theme: {
        background: '#000000', foreground: '#cdd6f4', cursor: '#f5e0dc',
        selectionBackground: '#585b7066',
        black: '#45475a', red: '#f38ba8', green: '#a6e3a1',
        yellow: '#f9e2af', blue: '#89b4fa', magenta: '#f5c2e7',
        cyan: '#94e2d5', white: '#bac2de',
        brightBlack: '#585b70', brightRed: '#f38ba8', brightGreen: '#a6e3a1',
        brightYellow: '#f9e2af', brightBlue: '#89b4fa', brightMagenta: '#f5c2e7',
        brightCyan: '#94e2d5', brightWhite: '#a6adc8',
      }
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon());
    termRef.current = term;
    fitRef.current = fit;

    if (containerRef.current) {
      term.open(containerRef.current);
      fit.fit();
      connect(term, fit);
      const ro = new ResizeObserver(() => fit.fit());
      ro.observe(containerRef.current);
      resizeRef.current = ro;
    }

    return () => {
      resizeRef.current?.disconnect();
      wsRef.current?.close();
      term.dispose();
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function connect(term: XTerm, fit: FitAddon) {
    setError('');
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/api/terminal`);
    wsRef.current = ws;

    ws.onopen = () => {
      setConnected(true);
      sendResize(ws, term);
    };
    ws.onmessage = (event) => term.write(event.data);
    ws.onclose = () => {
      setConnected(false);
      term.write(`\r\n\x1b[90m--- ${t('terminal.sessionEnded')} ---\x1b[0m\r\n`);
    };
    ws.onerror = () => {
      setError(t('terminal.wsFailed'));
      setConnected(false);
    };

    term.onData((data: string) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(data);
    });
    term.onResize(() => sendResize(ws, term));
  }

  function sendResize(ws: WebSocket, term: XTerm) {
    if (ws.readyState !== WebSocket.OPEN) return;
    const buf = new Uint8Array(4);
    buf[0] = (term.cols >> 8) & 0xff;
    buf[1] = term.cols & 0xff;
    buf[2] = (term.rows >> 8) & 0xff;
    buf[3] = term.rows & 0xff;
    ws.send(buf.buffer);
  }

  function reconnect() {
    wsRef.current?.close();
    termRef.current?.clear();
    if (termRef.current && fitRef.current) connect(termRef.current, fitRef.current);
  }

  return (
    <div className="terminal-page">
      <div className="terminal-header">
        <h1>{t('terminal.title')}</h1>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span className={`badge ${connected ? 'online' : 'inactive'}`}>{connected ? t('terminal.connected') : t('terminal.disconnected')}</span>
          {!connected && <button className="btn secondary" onClick={reconnect}>{t('terminal.reconnect')}</button>}
        </div>
      </div>
      {error && <div className="error-msg" style={{ marginBottom: 8 }}>{error}</div>}
      <div className="terminal-container" ref={containerRef} />
    </div>
  );
}
