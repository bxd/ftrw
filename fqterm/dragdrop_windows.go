//go:build windows

package main

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// dragdrop_windows.go：出站 OLE 拖放。前端「左键按住行 + 移动超阈」调 DragPaths，
// 这里造 IDataObject(CF_HDROP) + IDropSource 交 DoDragDrop，目标应用（Explorer 等）
// 自行决定复制/移动/链接，与从 Explorer 拖出的语义一致。
//
// 线程模型：DoDragDrop 的模态循环靠所在线程收鼠标输入，后台 STA 线程收不到
// （按住拖动毫无反应），与 TrackPopupMenu「闪现即消失」同根。故过桥到持有主窗的
// Wails 主线程执行（application.InvokeSync，同 shellmenu 的根治方案）。
// Wails 主线程 COM 已被托盘等初始化成 STA，但 OLE 层没初始化：DoDragDrop 依赖
// OLE 初始化，光 CoInitializeEx 会报 0x800401F0。故桥内用 OleInitialize（容 S_FALSE，
// go-ole 的 CoInitializeEx 把 S_FALSE 当错误抛「函数不正确」，不能用）+ 成对 OleUninitialize。
// 模态循环自带消息泵，阻塞期间主窗消息照常派发。并发拖拽用原子闸拒掉。
//
// vtable 槽号已对照 Windows SDK ObjIdl.h / OleIdl.h 核实（继 IUnknown 0/1/2 后）：
// IDataObject: GetData=3 GetDataHere=4 QueryGetData=5 GetCanonicalFormatEtc=6
// SetData=7 EnumFormatEtc=8 DAdvise=9 DUnadvise=10 EnumDAdvise=11
// IDropSource: QueryContinueDrag=3 GiveFeedback=4
// IEnumFORMATETC: Next=3 Skip=4 Reset=5 Clone=6

var (
	modOle32            = syscall.NewLazyDLL("ole32.dll")
	procDoDragDrop      = modOle32.NewProc("DoDragDrop")
	procOleInitialize   = modOle32.NewProc("OleInitialize")
	procOleUninitialize = modOle32.NewProc("OleUninitialize")
	procGlobalAlloc     = modKernel32.NewProc("GlobalAlloc")
	procGlobalLock      = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock    = modKernel32.NewProc("GlobalUnlock")
	procGlobalFree      = modKernel32.NewProc("GlobalFree")
)

const (
	cfHDrop         = 15 // CF_HDROP
	tymedHGlobal    = 1  // TYMED_HGLOBAL
	dvaspectContent = 1  // DVASPECT_CONTENT
	datadirGet      = 1  // DATADIR_GET
	gmemMoveable    = 0x0002
	gmemZeroInit    = 0x0040
	mkLButton       = 0x0001

	eNoInterface        = 0x80004002
	eNotImpl            = 0x80004001
	eOutOfMemory        = 0x8007000E
	dvEFormatEtc        = 0x80040064
	dataESameFormatEtc  = 0x00040130
	dragdropSDrop       = 0x00040100
	dragdropSCancel     = 0x00040101
	dragdropSUseDefault = 0x00040102

	dropeffectCopy = 1
	dropeffectMove = 2
	dropeffectLink = 4
)

var (
	iidIUnknown       = ole.GUID{Data1: 0x00000000, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIDataObject    = ole.GUID{Data1: 0x0000010E, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIDropSource    = ole.GUID{Data1: 0x00000121, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIEnumFormatEtc = ole.GUID{Data1: 0x00000103, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// ---- 与 C 布局对齐的结构（x64） ----

// formatEtc 对齐 FORMATETC：WORD cfFormat + 6 pad + ptr + 3×DWORD。
type formatEtc struct {
	cfFormat uint16
	_        [6]byte
	ptd      uintptr
	dwAspect uint32
	lindex   int32
	tymed    uint32
}

// stgMedium 对齐 STGMEDIUM：DWORD tymed + pad + union(取 hGlobal) + punk。
type stgMedium struct {
	tymed   uint32
	_       uint32
	hGlobal uintptr
	punk    uintptr
}

// dropFiles 对齐 DROPFILES（20 字节），后接双 NUL 结尾的宽串列表。
type dropFiles struct {
	pFiles uint32
	pt     struct{ x, y int32 }
	fNC    int32
	fWide  int32
}

// ---- Go 侧 COM 对象：首字段 vtable 指针，回调经 this 还原 ----

type comHeader struct {
	vtbl *uintptr
	refs int32
}

type dragDataObject struct {
	comHeader
	hdrop []byte
	enum  *dragEnum // EnumFormatEtc 惰性造，随 data 对象存活
}

type dragSource struct{ comHeader }

type dragEnum struct {
	comHeader
	pos int32
}

// NewCallback 槽位不可回收，全部包级单例（沿用 kbdhook 的约定）。
var (
	cbAddRef  = syscall.NewCallback(dragAddRef)
	cbRelease = syscall.NewCallback(dragRelease)

	cbDataQI      = syscall.NewCallback(dataQueryInterface)
	cbDataGet     = syscall.NewCallback(dataGetData)
	cbDataGetHere = syscall.NewCallback(dataGetDataHere)
	cbDataQuery   = syscall.NewCallback(dataQueryGetData)
	cbDataCanon   = syscall.NewCallback(dataGetCanonicalFormatEtc)
	cbDataSet     = syscall.NewCallback(dataSetData)
	cbDataEnum    = syscall.NewCallback(dataEnumFormatEtc)
	cbDataAdv     = syscall.NewCallback(dataAdvise)
	cbDataUnadv   = syscall.NewCallback(dataUnadvise)
	cbDataEnumAdv = syscall.NewCallback(dataEnumDAdvise)

	cbSrcQI  = syscall.NewCallback(srcQueryInterface)
	cbSrcQCD = syscall.NewCallback(srcQueryContinueDrag)
	cbSrcGF  = syscall.NewCallback(srcGiveFeedback)

	cbEnumQI    = syscall.NewCallback(enumQueryInterface)
	cbEnumNext  = syscall.NewCallback(enumNext)
	cbEnumSkip  = syscall.NewCallback(enumSkip)
	cbEnumReset = syscall.NewCallback(enumReset)
	cbEnumClone = syscall.NewCallback(enumClone)
)

var (
	dataVtbl = [12]uintptr{cbDataQI, cbAddRef, cbRelease, cbDataGet, cbDataGetHere, cbDataQuery, cbDataCanon, cbDataSet, cbDataEnum, cbDataAdv, cbDataUnadv, cbDataEnumAdv}
	srcVtbl  = [5]uintptr{cbSrcQI, cbAddRef, cbRelease, cbSrcQCD, cbSrcGF}
	enumVtbl = [7]uintptr{cbEnumQI, cbAddRef, cbRelease, cbEnumNext, cbEnumSkip, cbEnumReset, cbEnumClone}
)

func dragAddRef(this unsafe.Pointer) uintptr {
	return uintptr(atomic.AddInt32(&(*comHeader)(this).refs, 1))
}

func dragRelease(this unsafe.Pointer) uintptr {
	return uintptr(atomic.AddInt32(&(*comHeader)(this).refs, -1))
}

// comQueryInterface 命中 IID 表就回 this 并 AddRef，否则 E_NOINTERFACE。
func comQueryInterface(this unsafe.Pointer, riid, ppv unsafe.Pointer, iids []ole.GUID) uintptr {
	out := (*unsafe.Pointer)(ppv)
	g := (*ole.GUID)(riid)
	for _, iid := range iids {
		if *g == iid {
			*out = this
			dragAddRef(this)
			return 0
		}
	}
	*out = nil
	return eNoInterface
}

func dataQueryInterface(this, riid, ppv unsafe.Pointer) uintptr {
	return comQueryInterface(this, riid, ppv, []ole.GUID{iidIUnknown, iidIDataObject})
}

func srcQueryInterface(this, riid, ppv unsafe.Pointer) uintptr {
	return comQueryInterface(this, riid, ppv, []ole.GUID{iidIUnknown, iidIDropSource})
}

func enumQueryInterface(this, riid, ppv unsafe.Pointer) uintptr {
	return comQueryInterface(this, riid, ppv, []ole.GUID{iidIUnknown, iidIEnumFormatEtc})
}

// dataGetData 回 CF_HDROP 的 HGLOBAL（每次新 alloc，接收方负责 free）。
func dataGetData(this unsafe.Pointer, fe *formatEtc, med *stgMedium) uintptr {
	d := (*dragDataObject)(this)
	if fe.cfFormat != cfHDrop || fe.tymed&tymedHGlobal == 0 {
		return dvEFormatEtc
	}
	h, _, _ := procGlobalAlloc.Call(gmemMoveable|gmemZeroInit, uintptr(len(d.hdrop)))
	if h == 0 {
		return eOutOfMemory
	}
	r, _, _ := procGlobalLock.Call(h)
	p := *(*unsafe.Pointer)(unsafe.Pointer(&r)) // vet 认可的 uintptr→Pointer 重解释
	if p == nil {
		procGlobalFree.Call(h)
		return eOutOfMemory
	}
	copy(unsafe.Slice((*byte)(p), len(d.hdrop)), d.hdrop)
	procGlobalUnlock.Call(h)
	med.tymed = tymedHGlobal
	med.hGlobal = h
	med.punk = 0
	return 0
}

func dataGetDataHere(this unsafe.Pointer, fe *formatEtc, med *stgMedium) uintptr {
	return eNotImpl
}

func dataQueryGetData(this unsafe.Pointer, fe *formatEtc) uintptr {
	if fe.cfFormat == cfHDrop && fe.tymed&tymedHGlobal != 0 {
		return 0
	}
	return dvEFormatEtc
}

func dataGetCanonicalFormatEtc(this unsafe.Pointer, feIn, feOut *formatEtc) uintptr {
	if feOut != nil {
		feOut.ptd = 0
	}
	return dataESameFormatEtc
}

func dataSetData(this unsafe.Pointer, fe *formatEtc, med *stgMedium, release int32) uintptr {
	return eNotImpl
}

func dataEnumFormatEtc(this unsafe.Pointer, direction uint32, ppEnum unsafe.Pointer) uintptr {
	out := (*unsafe.Pointer)(ppEnum)
	if direction != datadirGet {
		*out = nil
		return eNotImpl
	}
	d := (*dragDataObject)(this)
	if d.enum == nil {
		d.enum = &dragEnum{comHeader: comHeader{vtbl: &enumVtbl[0]}}
	}
	*out = unsafe.Pointer(d.enum)
	dragAddRef(*out)
	return 0
}

func dataAdvise(this unsafe.Pointer, fe *formatEtc, advf uint32, sink unsafe.Pointer, conn *uint32) uintptr {
	return eNotImpl
}

func dataUnadvise(this unsafe.Pointer, conn uint32) uintptr { return eNotImpl }

func dataEnumDAdvise(this unsafe.Pointer, ppEnum unsafe.Pointer) uintptr {
	*(*unsafe.Pointer)(ppEnum) = nil
	return eNotImpl
}

// ---- IDropSource：Esc 取消、松左键放下 ----

func srcQueryContinueDrag(this unsafe.Pointer, fEscape int32, keys uint32) uintptr {
	if fEscape != 0 {
		return dragdropSCancel
	}
	if keys&mkLButton == 0 {
		return dragdropSDrop
	}
	return 0
}

func srcGiveFeedback(this unsafe.Pointer, effect uint32) uintptr {
	return dragdropSUseDefault
}

// ---- IEnumFORMATETC：只报一种格式 CF_HDROP ----

func enumNext(this unsafe.Pointer, celt uint32, rgelt unsafe.Pointer, fetched *uint32) uintptr {
	e := (*dragEnum)(this)
	if celt >= 1 && e.pos == 0 {
		*(*formatEtc)(rgelt) = formatEtc{cfFormat: cfHDrop, dwAspect: dvaspectContent, lindex: -1, tymed: tymedHGlobal}
		e.pos = 1
		if fetched != nil {
			*fetched = 1
		}
		return 0
	}
	if fetched != nil {
		*fetched = 0
	}
	return 1 // S_FALSE
}

func enumSkip(this unsafe.Pointer, celt uint32) uintptr {
	e := (*dragEnum)(this)
	e.pos += int32(celt)
	if e.pos > 1 {
		e.pos = 1
		return 1
	}
	return 0
}

func enumReset(this unsafe.Pointer) uintptr {
	(*dragEnum)(this).pos = 0
	return 0
}

func enumClone(this unsafe.Pointer, ppEnum unsafe.Pointer) uintptr {
	*(*unsafe.Pointer)(ppEnum) = nil
	return eNotImpl
}

// ---- HDROP 与发起 ----

// buildHDrop 造 DROPFILES + 宽串列表（每串 NUL 结尾 + 总尾 NUL）。
func buildHDrop(paths []string) []byte {
	df := dropFiles{pFiles: uint32(unsafe.Sizeof(dropFiles{})), fWide: 1}
	buf := append([]byte{}, unsafe.Slice((*byte)(unsafe.Pointer(&df)), unsafe.Sizeof(df))...)
	for _, p := range paths {
		u, err := syscall.UTF16FromString(p)
		if err != nil {
			continue
		}
		for _, c := range u {
			buf = append(buf, byte(c), byte(c>>8))
		}
	}
	return append(buf, 0, 0)
}

var dragBusy atomic.Bool

// dragPathsNative 把 DoDragDrop 过桥到 Wails 主线程跑，阻塞到放下/取消。
func dragPathsNative(paths []string) error {
	if !dragBusy.CompareAndSwap(false, true) {
		return fmt.Errorf("已有拖拽在进行")
	}
	defer dragBusy.Store(false)

	data := &dragDataObject{comHeader: comHeader{vtbl: &dataVtbl[0]}, hdrop: buildHDrop(paths)}
	src := &dragSource{comHeader: comHeader{vtbl: &srcVtbl[0]}}

	var hr uintptr
	var initErr error
	application.InvokeSync(func() {
		r, _, _ := procOleInitialize.Call(0)
		if int32(r) < 0 {
			initErr = hrErr(r, "OleInitialize")
			return
		}
		defer procOleUninitialize.Call()
		var effect uint32
		hr, _, _ = procDoDragDrop.Call(
			uintptr(unsafe.Pointer(data)),
			uintptr(unsafe.Pointer(src)),
			dropeffectCopy|dropeffectMove|dropeffectLink,
			uintptr(unsafe.Pointer(&effect)),
		)
	})
	runtime.KeepAlive(data)
	runtime.KeepAlive(src)
	log.Printf("DoDragDrop 收尾 hr=0x%X paths=%d", hr, len(paths))
	if initErr != nil {
		return fmt.Errorf("COM 初始化失败：%v", initErr)
	}
	if int32(hr) < 0 {
		return hrErr(hr, "DoDragDrop")
	}
	return nil
}
