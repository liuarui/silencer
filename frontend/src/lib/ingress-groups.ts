/**
 * 词表「按入口分组」的共用工具（首页今日词明细 + 统计页敏感词排行榜）。
 *
 * 为什么必须分组同屏、而不是过滤或单开一栏（产品决策，SPEC §5.2(3)）：
 * - **词榜不是排行榜，是日志的导航索引**：首页词条点击直接跳 `/logs?q=…`，
 *   所以词条上的 `×N` 必须与日志页查出来的条数同口径。一旦「首页只看代理、
 *   日志页看全部」，点进去数字对不上，用户会判定"统计坏了"。
 * - **"默认只看代理"= 主动隐藏威胁**：扩展链路的输入正是产品要拦的东西，
 *   默认不显示等于「我拦了但不告诉你拦了什么」。
 * - **污染的根因是量级不对称**（浏览器流量 ≫ CLI），不是"混在一起"这个动作；
 *   各组各取 Top N 才能让代理组的业务词（公司名/客户名/密钥）不被邮箱/电话挤下去。
 */
import type { TodayStats, IngressWordGroup } from '@/types/api'

export type Ingress = 'proxy' | 'ext'

export interface IngressWordList {
  ingress: Ingress
  /** 该入口下的命中总数（组头计数，与 Top N 截断无关） */
  total: number
  items: { label: string; word: string; original?: string; display?: 'plaintext' | 'placeholder'; count: number }[]
}

const EMPTY_GROUP: IngressWordGroup = {
  by_label: {},
  by_label_words: {},
  top_words: [],
  label_total: 0,
}

/**
 * 规范化按入口分组的词表。
 *
 * 恒返回 **proxy → ext 两个组**（顺序固定，不随数据漂移），空组也会返回，
 * 由调用方决定是否渲染——因为「ext 组为空」有两种完全不同的含义：
 *   · `ext_bridge_enabled=false` → 整个扩展功能没开，整组不渲染；
 *   · `ext_bridge_enabled=true` 但该组为空 → **恰恰是「扩展装了但一条都没走通」的信号**，
 *     必须保留组头并提示"本周期无记录"，不能静默藏掉。
 *
 * 老数据（`ingress` 列为空）在服务端已按 `proxy` 解读，这里不需要再兜底。
 */
export function groupWordsByIngress(
  stats: TodayStats | undefined,
  limit = 20,
): IngressWordList[] {
  const groups = stats?.words_by_ingress
  // Older gateways returned only top_words.  Keep those records visible in
  // the proxy bucket until the new grouped response is available; otherwise
  // the dashboard can show a non-zero masked_items card beside two empty
  // ingress sections.
  const legacyProxy: IngressWordGroup | undefined = !groups?.proxy && stats?.top_words?.length
    ? {
        by_label: stats.by_label ?? {},
        by_label_words: {},
        top_words: stats.top_words,
        label_total: Number(stats.masked_items ?? stats.top_words.reduce((sum, item) => sum + Number(item.count ?? 0), 0)),
      }
    : undefined
  const pick = (ingress: Ingress): IngressWordList => {
    const g = (groups?.[ingress] as IngressWordGroup | undefined) ?? (ingress === 'proxy' ? legacyProxy : undefined) ?? EMPTY_GROUP
    return {
      ingress,
      total: Number(g.label_total ?? 0),
      items: (g.top_words ?? []).slice(0, limit),
    }
  }
  return [pick('proxy'), pick('ext')]
}

/** 由入口值构造日志页跳转查询串片段（保证「词条 ×N」与日志条数同口径）。 */
export function ingressQuery(ingress: Ingress): string {
  return `&ingress=${ingress}`
}
