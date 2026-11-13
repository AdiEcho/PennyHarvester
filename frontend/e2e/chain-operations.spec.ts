import { expect, test } from '@playwright/test'

test.beforeEach(async ({ request }) => {
  await request.post('/api/v1/__reset')
})

test('resolves an unknown chain operation with audit evidence', async ({ page, request }) => {
  await page.goto('/')
  await page.getByRole('button', { name: /链上操作/ }).click()

  const resolveButtons = page.getByRole('button', { name: '核对', exact: true })
  await expect(resolveButtons.first()).toBeVisible()
  await resolveButtons.first().click()

  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog.getByLabel(/交易哈希/)).toHaveAttribute('readonly', '')
  await dialog.getByLabel(/核对人/).fill('e2e-operator')
  await dialog.getByLabel(/核对说明/).fill('checked polygonscan receipt: success')
  await dialog.getByRole('button', { name: '记录核对结果' }).click()
  await expect(dialog).toBeHidden()

  await expect
    .poll(
      async () => {
        const response = await request.get('/api/v1/__state')
        const state = (await response.json()) as {
          operations: Array<{ status: string; resolved_by?: string; resolution_note?: string }>
        }
        return state.operations[0]
      },
      { timeout: 5_000 },
    )
    .toMatchObject({
      status: 'confirmed',
      resolved_by: 'e2e-operator',
      resolution_note: 'checked polygonscan receipt: success',
    })

  await expect(page.getByText('核对结果已记录')).toBeVisible()
})

test('shows the mobile card and keeps the resolve dialog reachable', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/')
  await page.getByRole('button', { name: '打开导航菜单' }).click()
  await page.getByRole('dialog').getByRole('button', { name: /链上操作/ }).click()

  await expect(page.locator('table')).toBeHidden()
  const mobile = page.locator('div.grid.gap-3.sm\\:grid-cols-2.lg\\:hidden')
  const mobileResolve = mobile.getByRole('button', { name: '核对', exact: true })
  await expect(mobileResolve).toBeVisible()

  await mobileResolve.click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByRole('dialog').getByLabel(/核对人/)).toBeVisible()
})

test('venue-wide approvals are reachable from the all-accounts scope', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: /链上操作/ }).click()

  await page.getByLabel('选择账户').click()
  await page.getByRole('option', { name: '全部账户 / 场所级' }).click()

  const venueRow = page.locator('tr').filter({ hasText: '#2' })
  await expect(venueRow).toContainText('场所级授权')
  await venueRow.getByRole('button', { name: '核对', exact: true }).click()

  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog.getByLabel(/核对人/)).toBeVisible()
})
