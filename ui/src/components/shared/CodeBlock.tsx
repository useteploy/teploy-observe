interface Props {
  code: string;
  language?: string;
  maxHeight?: string;
}

export default function CodeBlock({ code, maxHeight = "400px" }: Props) {
  return (
    <pre class="obs-code-block" style={{ maxHeight }}>
      <code>{code}</code>
    </pre>
  );
}
