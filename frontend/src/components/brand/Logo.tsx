/** Silencer mark: a signal crossing a privacy gate before it reaches the network. */
import { cn } from '@/lib/utils'

export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 36 36" fill="none" className={cn('h-8 w-8', className)} aria-hidden="true">
      <rect x="1" y="1" width="34" height="34" rx="11" fill="#111827" />
      <path
        d="M6.5 18h4l2.5-5.5 4.5 11 2.4-5.5h2.4"
        stroke="#c4b5fd"
        strokeWidth="2.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <rect x="23" y="9" width="3.2" height="18" rx="1.6" fill="#5eead4" />
      <path d="M28.3 18h2.2" stroke="#f8fafc" strokeWidth="2" strokeLinecap="round" />
      <circle cx="31" cy="18" r="1.6" fill="#f8fafc" />
    </svg>
  )
}
