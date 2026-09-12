.syntax unified
.cpu cortex-m3
.thumb

.equ RGB_SET_COLOR_IMPL, 0x080091d5
.equ RGB_SET_ALL_IMPL,   0x08009211
.equ RGB_FLUSH_IMPL,     0x0800922d
.equ RGB_MATRIX_OFF_FLAG, 0x200023a6

.section .text
.global direct_handler
.type direct_handler, %function
.thumb_func
direct_handler:
    push    {r4, lr}
    mov     r4, r0
    ldrb    r1, [r4, #0]
    cmp     r1, #7
    bne     direct_fallback
    ldrb    r1, [r4, #1]
    cmp     r1, #0
    bne     direct_fallback
    ldrb    r1, [r4, #2]
    cmp     r1, #1
    bne     direct_fallback
    ldrb    r0, [r4, #3]
    cmp     r0, #27
    bcs     direct_fallback
    ldrb    r1, [r4, #4]
    ldrb    r2, [r4, #5]
    ldrb    r3, [r4, #6]
 .global direct_rgb_set_call
direct_rgb_set_call:
    bl      direct_handler
    pop     {r4, lr}
    b.w     direct_flush

direct_fallback:
    movs    r1, #0xff
    strb    r1, [r4, #0]
    pop     {r4, pc}

.global direct_flush
.type direct_flush, %function
.thumb_func
direct_flush:
    ldr     r3, =RGB_FLUSH_IMPL
    bx      r3

.global direct_guard_color
.type direct_guard_color, %function
.thumb_func
direct_guard_color:
    ldr     r3, =RGB_MATRIX_OFF_FLAG
    ldrb    r3, [r3]
    cmp     r3, #0
    bne     direct_guard_return
    ldr     r3, =RGB_SET_COLOR_IMPL
    bx      r3
direct_guard_return:
    bx      lr

.global direct_guard_all
.type direct_guard_all, %function
.thumb_func
direct_guard_all:
    ldr     r3, =RGB_MATRIX_OFF_FLAG
    ldrb    r3, [r3]
    cmp     r3, #0
    bne     direct_guard_return_all
    ldr     r3, =RGB_SET_ALL_IMPL
    bx      r3
direct_guard_return_all:
    bx      lr
