export function truncateImage(image: string, maxLen = 60): string {
  if (image.length <= maxLen) return image;
  const keep = Math.floor((maxLen - 3) / 2);
  return image.slice(0, keep) + "..." + image.slice(-keep);
}

export function formatRelativeTime(dateStr: string): string {
  try {
    const date = new Date(dateStr);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    if (diffMs < 0) return "just now";

    const mins = Math.floor(diffMs / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;

    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours}h ago`;

    const days = Math.floor(hours / 24);
    if (days < 7) return `${days}d ago`;

    return date.toLocaleDateString();
  } catch {
    return dateStr;
  }
}
