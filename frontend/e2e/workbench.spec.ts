import { expect, test } from '@playwright/test'

test.beforeEach(async ({ request }) => {
  await request.post('/api/v1/__reset')
})

test('renders the workbench with the selected account', async ({ page }) => {
  await page.goto('/')

  await expect(page.getByText('PennyHarvester').first()).toBeVisible()
  await expect(page.getByText('E2E 模拟账户').first()).toBeVisible()
  await expect(page.getByText('模拟演练模式', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: /账户总览/ })).toBeVisible()
})
