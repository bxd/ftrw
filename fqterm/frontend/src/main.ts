import { createApp } from 'vue'
import App from './App.vue'
import PreviewApp from './PreviewApp.vue'

// 同一个前端产物挂在两个窗口上：主窗口，以及后端 SetURL 出来的预览窗口。
// 预览窗口靠地址上的 ?view=preview 分流（那个地址写死在 preview.go 的
// previewPageURL），不这么做的话它会连终端一起再挂载一遍。
const view = new URLSearchParams(window.location.search).get('view')

if (view === 'preview') {
  createApp(PreviewApp).mount('#app')
} else {
  createApp(App).mount('#app')
}
