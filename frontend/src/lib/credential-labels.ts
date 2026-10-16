/**
 * 凭据类标签集合，供环境变量导入器限制凭据的分类目标。
 *
 * 日志与统计会按用户要求显示脱敏命中的逐词原文；该集合不用于隐藏命中内容。
 * 仍将凭据导入限制在稳定类别中，避免用户将其误分类为普通业务词。
 */
export const CREDENTIAL_LABELS = [
  'API_KEY',
  'TOKEN',
  'SECRET',
  'ACCESS_KEY',
  'JWT',
  'CONNSTR',
  'PRIVATE_KEY',
] as const

export type CredentialLabel = (typeof CREDENTIAL_LABELS)[number]

/** 便于 `Set` 场景的查表用法（`.has(label)`）。 */
export const CRED_LABELS: ReadonlySet<string> = new Set(CREDENTIAL_LABELS)
