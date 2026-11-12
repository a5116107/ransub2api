import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const frontendRoot = resolve(__dirname, '../../../..')
const stripeConsumers = [
  'src/views/user/StripePaymentView.vue',
  'src/views/user/StripePopupView.vue',
  'src/components/payment/StripePaymentInline.vue',
]

function readFrontendFile(path: string): string {
  return readFileSync(resolve(frontendRoot, path), 'utf8')
}

describe('Payment SDK lazy-loading contract', () => {
  it.each(stripeConsumers)('%s uses the side-effect-free Stripe loader', (path) => {
    const source = readFrontendFile(path)

    expect(source).toContain("await import('@stripe/stripe-js/pure')")
    expect(source).not.toMatch(/await import\(['"]@stripe\/stripe-js['"]\)/)
  })

  it('loads Airwallex only after recovering payment parameters', () => {
    const source = readFrontendFile('src/views/user/AirwallexPaymentView.vue')
    const missingParameters = source.indexOf('if (!snapshot)')
    const importPosition = source.indexOf("await import('@airwallex/components-sdk')")

    expect(missingParameters).toBeGreaterThan(-1)
    expect(importPosition).toBeGreaterThan(missingParameters)
    expect(source.slice(missingParameters, importPosition)).toContain('return')
    expect(source).not.toMatch(/import\s+(?:\{|\*|\w).*from\s+['"]@airwallex\/components-sdk['"]/)
  })

  it.each([
    { packageName: '@stripe/stripe-js', chunkName: 'vendor-stripe' },
    { packageName: '@airwallex/components-sdk', chunkName: 'vendor-airwallex' },
  ])('keeps $packageName out of the shared vendor chunk', ({ packageName, chunkName }) => {
    const viteConfig = readFrontendFile('vite.config.ts')
    const paymentRule = viteConfig.indexOf(`id.includes('/${packageName}/')`)
    const miscFallback = viteConfig.indexOf("return 'vendor-misc'")

    expect(paymentRule).toBeGreaterThan(-1)
    expect(viteConfig.slice(paymentRule, miscFallback)).toContain(`return '${chunkName}'`)
    expect(paymentRule).toBeLessThan(miscFallback)
  })
})
