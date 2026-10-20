import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getLogs } from '@/api/logs'
import type { SilencerEvent } from '@/types/api'

/** Latest retained event window stored per filter combination. */
export interface LogsCache {
  list: SilencerEvent[]
  tail: string[]
  windowLimited: boolean
}

const MAX_LOG_LIST = 2000

export interface UseLogsQueryOptions {
  filterType: string
  sensitive: boolean
  hideNoise: boolean
  q: string
  fulltext: boolean
  /** Filter value stored in the cache key, including the "all" sentinel. */
  ingress: string
  /** Normalized API filters; UI sentinels are resolved by the page. */
  requestType?: string
  requestIngress?: 'proxy' | 'ext' | 'capture'
  hidden: boolean
  onTail: (tail: string[]) => void
}

/**
 * Replace the retained event snapshot on each poll. Cached filters remain
 * immediately available on remount; event identities do not depend on timestamps.
 */
export function useLogsQuery({
  filterType,
  sensitive,
  hideNoise,
  q,
  fulltext,
  ingress,
  requestType,
  requestIngress,
  hidden,
  onTail,
}: UseLogsQueryOptions) {
  const logsKey = useMemo(
    () => ['logs', { filterType, sensitive, hideNoise, q, fulltext, ingress }] as const,
    [filterType, sensitive, hideNoise, q, fulltext, ingress],
  )

  const logsQuery = useQuery<LogsCache>({
    queryKey: logsKey,
    queryFn: async ({ signal }) => {
      // Completion can persist an event with an earlier timestamp; MySQL also
      // permits timestamp ties. A fresh bounded snapshot captures both and
      // removes cleared/expired rows instead of keeping them in an append cache.
      const response = await getLogs({
        latest: true,
        chains: true,
        limit: MAX_LOG_LIST,
        type: requestType,
        sensitive,
        hideNoise,
        q,
        fulltext,
        ingress: requestIngress,
        slim: true,
      }, signal)
      const tail = response.tail ?? []
      onTail(tail)
      return { list: response.events, tail, windowLimited: Boolean(response.window_limited || response.has_more) }
    },
    staleTime: 0,
    refetchOnMount: 'always',
    refetchInterval: hidden ? false : 3000,
    refetchIntervalInBackground: false,
  })

  return { logsKey, logsQuery }
}
