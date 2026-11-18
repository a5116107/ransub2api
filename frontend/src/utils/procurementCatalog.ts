import type { ProcurementCatalog, ProcurementCatalogSnapshot } from '@/api/admin/procurementCatalog'
import { procurementDecimalUnits, validateProcurementPolicy } from './procurementPolicy'

export function validateProcurementCatalog(catalog: ProcurementCatalog): string | null {
  if (!catalog || catalog.version !== 1) return 'unsupported'
  if (!Number.isSafeInteger(catalog.revision) || catalog.revision < 0 || !Array.isArray(catalog.buckets) || catalog.buckets.length > 64) return 'invalid'
  const codes = new Set<string>(), names = new Set<string>()
  for (const b of catalog.buckets) {
    if (!b || typeof b.code !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(b.code) || codes.has(b.code) ||
      typeof b.name !== 'string' || !b.name || b.name.trim() !== b.name || [...b.name].length > 100 || [...b.name].some(c => c.codePointAt(0)! < 32 || (c.codePointAt(0)! >= 127 && c.codePointAt(0)! <= 159)) || names.has(b.name) ||
      validateProcurementPolicy({ version: 1, revision: catalog.revision, min_rate: b.min_rate, max_rate: b.max_rate })) return 'invalid'
    codes.add(b.code); names.add(b.name)
  }
  const sorted = [...catalog.buckets].sort((a, b) => {
    const left = procurementDecimalUnits(a.min_rate) ?? 0n, right = procurementDecimalUnits(b.min_rate) ?? 0n
    return left < right ? -1 : left > right ? 1 : 0
  })
  for (let i = 1; i < sorted.length; i++) {
    const max = procurementDecimalUnits(sorted[i-1]!.max_rate), min = procurementDecimalUnits(sorted[i]!.min_rate) ?? 0n
    if (max === null || min < max) return 'overlap'
  }
  return null
}

export function validProcurementSnapshot(snapshot: ProcurementCatalogSnapshot): boolean {
  if (!snapshot || validateProcurementCatalog(snapshot.catalog) || !Array.isArray(snapshot.gaps) || snapshot.gaps.length > 65 ||
    (snapshot.catalog.revision === 0 && snapshot.catalog.buckets.length !== 0)) return false
  return snapshot.gaps.every(gap => gap && validateProcurementPolicy({ version: 1, revision: 0, min_rate: gap.min_rate, max_rate: gap.max_rate }) === null)
}
