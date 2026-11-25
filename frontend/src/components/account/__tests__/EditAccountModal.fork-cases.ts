import { describe, expect, it, type Mock } from 'vitest'
import { flushPromises, type VueWrapper } from '@vue/test-utils'
import type { Account } from '@/types'

// Keep fork cases outside the upstream spec's append-only test sections.
// Register against its fixtures so both suites use the same component setup.
interface Fixtures {
  mountModal: (account?: Account) => VueWrapper
  buildAccount: () => Account
  buildVertexAccount: () => Account
  updateAccountMock: Mock
  checkMixedChannelRiskMock: Mock
}

export function registerForkStreamingTests({ mountModal, buildAccount, buildVertexAccount, updateAccountMock, checkMixedChannelRiskMock }: Fixtures) {
  describe('EditAccountModal fork synchronous streaming', () => {
    it.each(['apikey', 'oauth', 'setup-token', 'service_account'] as const)('saves and clears synchronous upstream streaming for Anthropic %s', async (type) => {
      const account = { ...(type === 'service_account' ? buildVertexAccount() : buildAccount()), platform: 'anthropic', type, extra: { unrelated: 'preserve', anthropic_nonstream_to_stream: true } }
      updateAccountMock.mockReset().mockResolvedValue(account)
      checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
      const wrapper = mountModal(account)
      await flushPromises()
      const toggle = wrapper.get('[data-testid="anthropic-nonstream-to-stream"]')
      expect(toggle.attributes('aria-checked')).toBe('true')
      await toggle.trigger('click')
      await wrapper.get('#edit-account-form').trigger('submit.prevent')
      await flushPromises()
      expect(updateAccountMock.mock.calls[0]?.[1]?.extra).toMatchObject({ anthropic_nonstream_to_stream: false, unrelated: 'preserve' })
      await wrapper.setProps({ account: { ...account, extra: { unrelated: 'preserve' } } })
      expect(wrapper.get('[data-testid="anthropic-nonstream-to-stream"]').attributes('aria-checked')).toBe('false')
      await wrapper.get('[data-testid="anthropic-nonstream-to-stream"]').trigger('click')
      await wrapper.get('#edit-account-form').trigger('submit.prevent')
      await flushPromises()
      expect(updateAccountMock.mock.calls[1]?.[1]?.extra).toMatchObject({ anthropic_nonstream_to_stream: true, unrelated: 'preserve' })
    })

    it('does not offer Anthropic synchronous streaming to OpenAI accounts', () => {
      const wrapper = mountModal()
      expect(wrapper.find('[data-testid="anthropic-nonstream-to-stream"]').exists()).toBe(false)
    })

    it('offers and saves synchronous streaming after switching an API key to Anthropic', async () => {
      const account = buildAccount()
      updateAccountMock.mockReset().mockResolvedValue({ ...account, platform: 'anthropic' })
      checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
      const wrapper = mountModal(account)
      await flushPromises()
      await wrapper.get('[data-testid="edit-account-platform"]').setValue('anthropic')
      await flushPromises()
      await wrapper.get('[data-testid="anthropic-nonstream-to-stream"]').trigger('click')
      await wrapper.get('form#edit-account-form').trigger('submit.prevent')
      await flushPromises()
      expect(updateAccountMock).toHaveBeenCalledWith(1, expect.objectContaining({
        platform: 'anthropic', extra: expect.objectContaining({ anthropic_nonstream_to_stream: true })
      }))
    })

    it('clears Anthropic synchronous streaming when switching an API key to OpenAI', async () => {
      const account = { ...buildAccount(), platform: 'anthropic', extra: { anthropic_nonstream_to_stream: true } }
      updateAccountMock.mockReset().mockResolvedValue({ ...account, platform: 'openai' })
      checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
      const wrapper = mountModal(account)
      await flushPromises()
      await wrapper.get('[data-testid="edit-account-platform"]').setValue('openai')
      await flushPromises()
      expect(wrapper.find('[data-testid="anthropic-nonstream-to-stream"]').exists()).toBe(false)
      await wrapper.get('form#edit-account-form').trigger('submit.prevent')
      await flushPromises()
      expect(updateAccountMock.mock.calls[0]?.[1]?.platform).toBe('openai')
      expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.anthropic_nonstream_to_stream).toBeUndefined()
    })
  })
}
