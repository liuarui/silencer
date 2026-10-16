import { useQuery } from '@tanstack/react-query'
import { getStatus } from '@/api/proxy'
import type { ProxyStatus } from '@/types/api'

/** Shared polling policy for the status consumed by the shell and feature pages. */
export function useProxyStatus(hidden: boolean) {
  return useQuery<ProxyStatus>({
    queryKey: ['proxyStatus'],
    queryFn: getStatus,
    refetchInterval: (query) => {
      if (hidden) return false
      const status = query.state.data
      if (status === undefined || status.proxy_starting || status.proxy_stopping) return 1000
      return 5000
    },
    retry: 3,
  })
}
