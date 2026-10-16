import { create } from 'zustand'

// Display preference and sensitive searches live only in memory. A refresh or
// logout always returns the console to masked display.
export const usePrivacyStore = create<{
  showPlaintext: boolean
  setShowPlaintext: (value: boolean) => void
}>((set) => ({ showPlaintext: false, setShowPlaintext: (showPlaintext) => set({ showPlaintext }) }))

const searches = new Map<string, string>()
export function privateLogSearch(term: string): string {
  const id = crypto.randomUUID()
  searches.set(id, term)
  return id
}
export function readPrivateLogSearch(id: string | null): string {
  return id ? searches.get(id) ?? '' : ''
}
export function resetPrivacy() {
  searches.clear()
  usePrivacyStore.setState({ showPlaintext: false })
}
