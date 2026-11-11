#include <stdbool.h>
#include <stdlib.h>
void cageLoginLoop(void *machine);
void cageLoginStop(void);
void cageLoginEnable(bool enabled);
void *cageLoginImage(int *size);
void *cageLoginImageRegion(int x, int y, int width, int height, int *size);
bool cageLoginClick(int x, int y);
bool cageLoginScroll(int x, int y, int deltaX, int deltaY);
bool cageLoginKey(int code, bool shift);
bool cageLoginSelectAll(void);

bool cageDesktopOpen(void);
void cageDesktopClose(void);
bool cageDesktopVisible(void);
