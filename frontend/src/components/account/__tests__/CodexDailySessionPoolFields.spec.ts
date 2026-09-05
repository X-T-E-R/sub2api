import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import CodexDailySessionPoolFields from '../CodexDailySessionPoolFields.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('CodexDailySessionPoolFields', () => {
  it('edits numeric bounds and exposes an invalid-range error', async () => {
    const wrapper = mount(defineComponent({
      components: { CodexDailySessionPoolFields },
      data: () => ({ value: { enabled: false, min: 5, max: 10 } }),
      template: '<CodexDailySessionPoolFields v-model="value" id-prefix="test-pool" />'
    }))
    expect(wrapper.find('[data-testid="daily-session-pool-min"]').exists()).toBe(false)
    await wrapper.get('[role="switch"]').trigger('click')
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.get('label[for="test-pool-min"]').exists()).toBe(true)
    await wrapper.get('[data-testid="daily-session-pool-min"]').setValue('12')
    expect(wrapper.get('[role="alert"]').text()).toContain('invalidRange')
    await wrapper.get('[data-testid="daily-session-pool-max"]').setValue('12')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.vm.value).toEqual({ enabled: true, min: 12, max: 12 })
  })
})
