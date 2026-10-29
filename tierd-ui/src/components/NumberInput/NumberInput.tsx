import { useEffect, useState, type InputHTMLAttributes } from 'react';

type Props = Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'value' | 'onChange'> & {
  value: number;
  onChange: (value: number) => void;
};

// NumberInput is a controlled numeric input that can be typed into freely.
// A plain <input type="number"> bound to Number(e.target.value) turns an
// emptied field into 0 (or NaN) on every keystroke, so the user cannot clear
// it and type a new value. Here the text is kept as a draft while editing and
// onChange only fires for complete numbers; on blur the field shows the last
// committed value again.
export default function NumberInput({ value, onChange, onBlur, onFocus, ...rest }: Props) {
  const [draft, setDraft] = useState(String(value));
  const [editing, setEditing] = useState(false);

  useEffect(() => {
    if (!editing) setDraft(String(value));
  }, [value, editing]);

  return (
    <input
      {...rest}
      type="number"
      value={draft}
      onFocus={e => { setEditing(true); onFocus?.(e); }}
      onBlur={e => { setEditing(false); setDraft(String(value)); onBlur?.(e); }}
      onChange={e => {
        const text = e.target.value;
        setDraft(text);
        const n = Number(text);
        if (text.trim() !== '' && Number.isFinite(n)) onChange(n);
      }}
    />
  );
}
