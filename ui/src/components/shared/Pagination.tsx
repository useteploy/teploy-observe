interface Props {
  page: number;
  pageSize: number;
  resultCount: number;
  onPageChange: (page: number) => void;
}

export default function Pagination({ page, pageSize, resultCount, onPageChange }: Props) {
  const hasMore = resultCount === pageSize;
  if (page === 1 && !hasMore) return null;

  return (
    <div class="obs-pagination">
      <button
        class="obs-btn obs-btn--sm"
        disabled={page <= 1}
        onClick={() => onPageChange(page - 1)}
      >
        Previous
      </button>
      <span class="obs-pagination-info">Page {page}</span>
      <button
        class="obs-btn obs-btn--sm"
        disabled={!hasMore}
        onClick={() => onPageChange(page + 1)}
      >
        Next
      </button>
    </div>
  );
}
