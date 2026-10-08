export default function QueryFailure({ message, retry }: { message: string; retry: () => void }) {
  return <div class="obs-form-error" role="alert">
    <span>{message}</span>{" "}
    <button type="button" class="obs-btn obs-btn--sm" onClick={retry}>Retry</button>
  </div>;
}
