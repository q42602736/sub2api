import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  showError,
  showSuccess,
  nativeConfirm,
  autoReauthorizeBatch,
  getAutoReauthorizeBatchJob
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  nativeConfirm: vi.fn(() => true),
  autoReauthorizeBatch: vi.fn(),
  getAutoReauthorizeBatchJob: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      batchDelete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      autoReauthorizeBatch,
      getAutoReauthorizeBatchJob,
      bulkUpdate: vi.fn()
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token'
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const makeAccounts = (count: number) => Array.from({ length: count }, (_, index) => ({
  id: index + 1,
  name: `account-${index + 1}`,
  platform: 'grok',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  created_at: '2026-07-23T00:00:00Z',
  updated_at: '2026-07-23T00:00:00Z'
}))

const AccountBulkActionsBarStub = {
  props: ['selectedIds', 'totalResults', 'selectingAll', 'allResultsSelected'],
  emits: ['select-all-results', 'select-page', 'clear', 'auto-reauthorize'],
  template: `
    <div>
      <span data-test="selected-count">{{ selectedIds.length }}</span>
      <span data-test="total-results">{{ totalResults }}</span>
      <span data-test="all-results-selected">{{ String(allResultsSelected) }}</span>
      <button data-test="select-page" @click="$emit('select-page')">select page</button>
      <button data-test="select-all-results" @click="$emit('select-all-results')">select all</button>
      <button data-test="clear" @click="$emit('clear')">clear</button>
      <button data-test="auto-reauthorize" @click="$emit('auto-reauthorize')">auto reauthorize</button>
    </div>
  `
}

const ConfirmDialogStub = {
  props: ['show', 'title', 'message', 'confirmText', 'cancelText', 'danger'],
  emits: ['confirm', 'cancel'],
  template: `
    <div v-if="show" data-test="confirm-dialog">
      <span data-test="confirm-dialog-title">{{ title }}</span>
      <span data-test="confirm-dialog-message">{{ message }}</span>
      <button data-test="confirm-dialog-confirm" @click="$emit('confirm')">confirm</button>
      <button data-test="confirm-dialog-cancel" @click="$emit('cancel')">cancel</button>
    </div>
  `
}

const AccountTableFiltersStub = {
  emits: ['change'],
  template: '<button data-test="change-filter" @click="$emit(\'change\')">change filter</button>'
}

const mountView = () => mount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: {
        template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
      },
      DataTable: { props: ['data'], template: '<div data-test="data-table"></div>' },
      Pagination: true,
      ConfirmDialog: ConfirmDialogStub,
      AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
      AccountTableFilters: AccountTableFiltersStub,
      AccountBulkActionsBar: AccountBulkActionsBarStub,
      AccountActionMenu: true,
      ImportDataModal: true,
      ReAuthAccountModal: true,
      AccountTestModal: true,
      AccountStatsModal: true,
      BulkAutoReauthModal: {
        props: ['show', 'job'],
        template: '<div v-if="show" data-test="bulk-auto-reauth-modal"><span data-test="bulk-job-status">{{ job?.status }}</span></div>'
      },
      ScheduledTestsPanel: true,
      SyncFromCrsModal: true,
      TempUnschedStatusModal: true,
      ErrorPassthroughRulesModal: true,
      TLSFingerprintProfilesModal: true,
      CreateAccountModal: true,
      EditAccountModal: true,
      BulkEditAccountModal: true,
      PlatformTypeBadge: true,
      AccountCapacityCell: true,
      AccountStatusIndicator: true,
      AccountTodayStatsCell: true,
      AccountGroupsCell: true,
      AccountUsageCell: true,
      Icon: true
    }
  }
})

describe('admin AccountsView select all filtered results', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset()
    listWithEtag.mockReset()
    getBatchTodayStats.mockReset()
    getUpstreamBillingProbeSettings.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
    nativeConfirm.mockReset()
    nativeConfirm.mockReturnValue(true)
    autoReauthorizeBatch.mockReset()
    getAutoReauthorizeBatchJob.mockReset()

    listWithEtag.mockResolvedValue({
      notModified: true,
      etag: null,
      data: null
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
    vi.stubGlobal('confirm', nativeConfirm)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('selects all matching IDs in one commit and clears the selection when filters change', async () => {
    const allAccounts = makeAccounts(45)
    listAccounts.mockImplementation(async (_page: number, pageSize: number) => {
      if (pageSize === 1000) {
        return {
          items: allAccounts,
          total: 45,
          page: 1,
          page_size: 1000,
          pages: 1
        }
      }
      return {
        items: allAccounts.slice(0, 20),
        total: 45,
        page: 1,
        page_size: 20,
        pages: 3
      }
    })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('45')
    expect(wrapper.get('[data-test="total-results"]').text()).toBe('45')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('true')
    expect(listAccounts).toHaveBeenCalledWith(1, 1000, expect.objectContaining({
      lite: '1',
      include_scheduler_score: '0'
    }))

    await wrapper.get('[data-test="change-filter"]').trigger('click')

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('0')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('false')
  })

  it('keeps the original page selection when loading all results fails', async () => {
    const currentPage = makeAccounts(20)
    listAccounts.mockImplementation(async (_page: number, pageSize: number) => {
      if (pageSize === 1000) {
        throw new Error('load all failed')
      }
      return {
        items: currentPage,
        total: 45,
        page: 1,
        page_size: 20,
        pages: 3
      }
    })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="select-page"]').trigger('click')
    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('20')

    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('20')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('false')
    expect(showError).toHaveBeenCalledWith('admin.accounts.bulkActions.selectAllFailed')
  })

  it('starts and polls one batch automatic re-authorization job for the selection', async () => {
    const accounts = makeAccounts(2)
    listAccounts.mockResolvedValue({
      items: accounts,
      total: accounts.length,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const runningJob = {
      job_id: 'batch-1',
      status: 'running',
      total: 2,
      completed: 0,
      succeeded_count: 0,
      failed_count: 0,
      results: []
    }
    const finishedJob = {
      ...runningJob,
      status: 'succeeded',
      completed: 2,
      succeeded_count: 2,
      results: [
        { account_id: 1, account_name: 'account-1', status: 'succeeded', logs: [] },
        { account_id: 2, account_name: 'account-2', status: 'succeeded', logs: [] }
      ]
    }
    autoReauthorizeBatch.mockResolvedValueOnce(runningJob)
    getAutoReauthorizeBatchJob.mockResolvedValueOnce(finishedJob)

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="select-page"]').trigger('click')
    await wrapper.get('[data-test="auto-reauthorize"]').trigger('click')
    await flushPromises()

    expect(nativeConfirm).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="confirm-dialog-message"]').text()).toBe(
      'admin.accounts.bulkActions.confirmAutoReauthorize'
    )
    expect(autoReauthorizeBatch).not.toHaveBeenCalled()

    await wrapper.get('[data-test="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(autoReauthorizeBatch).toHaveBeenCalledWith([1, 2])
    expect(getAutoReauthorizeBatchJob).toHaveBeenCalledWith('batch-1')
    expect(wrapper.get('[data-test="bulk-job-status"]').text()).toBe('succeeded')
    expect(showSuccess).toHaveBeenCalled()
  })
})
