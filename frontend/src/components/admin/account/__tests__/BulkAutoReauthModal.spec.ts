import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import BulkAutoReauthModal from '../BulkAutoReauthModal.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

describe('BulkAutoReauthModal', () => {
  it('shows overall progress and each account flow logs', () => {
    const wrapper = mount(BulkAutoReauthModal, {
      props: {
        show: true,
        job: {
          job_id: 'batch-1',
          status: 'failed',
          total: 2,
          completed: 2,
          succeeded_count: 1,
          failed_count: 1,
          results: [
            {
              account_id: 11,
              account_name: '失败账号',
              status: 'failed',
              logs: [{ at: '2026-08-26T12:00:00Z', message: '正在请求验证码' }],
              error: '验证码流程失败'
            },
            {
              account_id: 12,
              account_name: '成功账号',
              status: 'succeeded',
              logs: [{ at: '2026-08-26T12:01:00Z', message: '授权成功' }]
            }
          ]
        }
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show', 'title'],
            template: '<div v-if="show"><h1>{{ title }}</h1><slot /><slot name="footer" /></div>'
          },
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('admin.accounts.oauth.openai.autoReauthBatchFailed')
    expect(wrapper.text()).toContain('失败账号')
    expect(wrapper.text()).toContain('正在请求验证码')
    expect(wrapper.text()).toContain('验证码流程失败')
    expect(wrapper.get('[role="progressbar"]').attributes('aria-valuenow')).toBe('100')
  })
})
