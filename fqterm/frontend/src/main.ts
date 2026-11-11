import { createApp } from 'vue'
import App from './App.vue'
import PreviewApp from './PreviewApp.vue'
import LauncherApp from './LauncherApp.vue'

// 同一个前端产物挂在三个窗口上：主窗口、后端 SetURL 出来的预览窗、以及隐藏常驻
// 的启动器窗。非主窗口靠地址上的 ?view= 分流（地址分别写死在 preview.go /
// launcher.go），不这么做的话它们会连终端一起再挂载一遍。
const view = new URLSearchParams(window.location.search).get('view')

if (view === 'preview') {
  createApp(PreviewApp).mount('#app')
} else if (view === 'launcher') {
  createApp(LauncherApp).mount('#app')
} else {
  createApp(App).mount('#app')
}
